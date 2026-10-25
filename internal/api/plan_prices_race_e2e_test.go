package api

import (
	"context"
	"database/sql"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/testutil"
)

// Races of the plan price writes (ADR-130 §4-§8, design §4.17 C3, C4, C9,
// C10, C11 and the teardown races of round 2). Each test stops one side at a
// lock the other side needs, with a row lock a test transaction holds, and
// checks the outcome of both orders where there are two. The statement text
// a side waits in is matched in pg_stat_activity (testutil.WaitForLockWaiter).

// The statements the sides wait in.
const (
	planLockSQL      = "FROM plans WHERE id = $1 FOR UPDATE"
	clocksLockSQL    = "FROM test_clocks ORDER BY id FOR SHARE"
	clockLockSQL     = "FROM test_clocks WHERE id = $1 FOR SHARE"
	teardownCustSQL  = "DELETE FROM customers WHERE test_clock_id"
	numberRowLockSQL = "tenant_settings"
)

// held is a row lock a test transaction holds until release.
type held struct {
	tx   *sql.Tx
	once sync.Once
}

// hold opens a transaction on the admin pool and runs lock in it.
func hold(t *testing.T, admin *sql.DB, lock string, args ...any) *held {
	t.Helper()
	tx, err := admin.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(lock, args...); err != nil {
		_ = tx.Rollback()
		t.Fatalf("%s: %v", lock, err)
	}
	h := &held{tx: tx}
	t.Cleanup(h.release)
	return h
}

func (h *held) release() { h.once.Do(func() { _ = h.tx.Rollback() }) }

// commitAndRelease commits what the holder wrote.
func (h *held) commit(t *testing.T) {
	t.Helper()
	h.once.Do(func() {
		if err := h.tx.Commit(); err != nil {
			t.Fatalf("commit the holder: %v", err)
		}
	})
}

type ppResp struct {
	code int
	body map[string]any
	raw  []byte
}

// async serves a request in a goroutine at real now.
func (r *ppRig) async(method, path string, body any) <-chan ppResp {
	out := make(chan ppResp, 1)
	go func() {
		code, b, raw := r.do(time.Time{}, method, path, body)
		out <- ppResp{code, b, raw}
	}()
	return out
}

func wait(t *testing.T, what string, ch <-chan ppResp) ppResp {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(30 * time.Second):
		t.Fatalf("%s never answered", what)
		return ppResp{}
	}
}

// TestE2E_PlanPrices_ValidFromReadAfterThePlanLock (C11): a version's
// valid_from is the database's wall clock read after the plan lock, not the
// transaction's start, so of two writes the one saved last takes effect last.
// The edit here waits for the plan lock; its version starts after the lock
// was let go.
//
// Mutation: valid_from = now() (the transaction's start), and the version
// starts before the release.
func TestE2E_PlanPrices_ValidFromReadAfterThePlanLock(t *testing.T) {
	r := newPPRig(t, "valid from")
	admin := testutil.AdminPool(t)
	calls := r.meter("calls", "sum")
	plan, ids := r.plan("pro", perUnit(calls, "1", nil))
	r.subscribe(time.Time{}, r.customer("pp-vf", ""), plan)

	h := hold(t, admin, `SELECT 1 FROM plans WHERE id = $1 FOR UPDATE`, plan)
	first := r.async(http.MethodPatch, "/v1/plans/"+plan+"/prices/"+ids[0], map[string]any{"mode": "per_unit", "unit_amount_cents": "2"})
	testutil.WaitForLockWaiter(t, context.Background(), admin, planLockSQL)
	second := r.async(http.MethodPatch, "/v1/plans/"+plan+"/prices/"+ids[0], map[string]any{"mode": "per_unit", "unit_amount_cents": "3"})
	testutil.WaitForLockWaiters(t, context.Background(), admin, planLockSQL, 2)
	time.Sleep(50 * time.Millisecond)
	var released time.Time
	if err := admin.QueryRow(`SELECT clock_timestamp()`).Scan(&released); err != nil {
		t.Fatal(err)
	}
	h.release()
	for _, ch := range []<-chan ppResp{first, second} {
		if got := wait(t, "edit", ch); got.code != http.StatusOK {
			t.Fatalf("edit: %d %s", got.code, got.raw)
		}
	}
	// The first version, from before the plan was used, starts at
	// '-infinity'; the two edits appended theirs.
	rows, err := admin.Query(`SELECT valid_from, unit_amount_cents::text FROM plan_price_versions
		WHERE price_id = $1 AND valid_from > '-infinity' ORDER BY valid_from`, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var froms []time.Time
	var rates []string
	for rows.Next() {
		var f time.Time
		var rate string
		if err := rows.Scan(&f, &rate); err != nil {
			t.Fatal(err)
		}
		froms, rates = append(froms, f), append(rates, rate)
	}
	if len(froms) != 2 {
		t.Fatalf("appended versions = %d (%v), want 2", len(froms), rates)
	}
	for i, f := range froms {
		if f.Before(released) {
			t.Errorf("version %d starts %v, before the plan lock was let go at %v", i+1, f, released)
		}
	}
	if !froms[1].After(froms[0]) {
		t.Errorf("the second saved version (%v) does not start after the first's (%v)", froms[1], froms[0])
	}
	if got := r.priceOf(plan, ids[0]); flowStr(got, "unit_amount_cents") != rates[1] {
		t.Errorf("content from now on = %v, want the last saved edit (%s)", got, rates[1])
	}
}

// TestE2E_PlanPrices_TyingAddsRace (C4): two adds that tie, sent together.
// The plan lock orders them, so the second sees the first and is refused.
//
// Mutation: drop the plan lock, and both adds are saved.
func TestE2E_PlanPrices_TyingAddsRace(t *testing.T) {
	r := newPPRig(t, "tying adds")
	admin := testutil.AdminPool(t)
	tokens := r.meter("tokens", "sum")
	plan, _ := r.plan("pro")
	h := hold(t, admin, `SELECT 1 FROM plans WHERE id = $1 FOR UPDATE`, plan)
	a := r.async(http.MethodPost, "/v1/plans/"+plan+"/prices", perUnit(tokens, "1", map[string]string{"model": "a"}))
	b := r.async(http.MethodPost, "/v1/plans/"+plan+"/prices", perUnit(tokens, "1", map[string]string{"region": "eu"}))
	testutil.WaitForLockWaiters(t, context.Background(), admin, planLockSQL, 2)
	h.release()
	got := []ppResp{wait(t, "add a", a), wait(t, "add b", b)}
	created, refused := 0, 0
	for _, g := range got {
		switch {
		case g.code == http.StatusCreated:
			created++
		case g.code == http.StatusConflict && flowStr(g.body, "error", "code") == "price_conflict":
			refused++
		default:
			t.Errorf("add: %d %s", g.code, g.raw)
		}
	}
	if created != 1 || refused != 1 {
		t.Errorf("created %d, refused %d; want one of each", created, refused)
	}
}

// TestE2E_PlanPrices_PriceWriteAgainstFirstSubscription (C3): a price write
// and the first subscription on a plan, in both orders. Either the write
// replaces the content before the subscription exists (effective now, and
// the first period bills it), or the subscription comes first and the write
// sees the plan used (effective next_period, the first period keeps the old
// content). Never a replace after use.
//
// Mutation: drop the plan's FOR UPDATE, and the write that waited for the
// subscription still replaces the content the open period bills.
func TestE2E_PlanPrices_PriceWriteAgainstFirstSubscription(t *testing.T) {
	t.Run("the write first", func(t *testing.T) {
		r := newPPRig(t, "c3 write first")
		admin := testutil.AdminPool(t)
		calls := r.meter("calls", "sum")
		plan, ids := r.plan("pro", perUnit(calls, "1", nil))
		r.clock("c3", time.Now().UTC()) // a clock, so the write waits at the clocks after its plan lock
		cust := r.customer("pp-c3w", "")
		h := hold(t, admin, `SELECT 1 FROM test_clocks WHERE tenant_id = $1 FOR UPDATE`, r.tenantID)
		edit := r.async(http.MethodPatch, "/v1/plans/"+plan+"/prices/"+ids[0], map[string]any{"mode": "per_unit", "unit_amount_cents": "5"})
		testutil.WaitForLockWaiter(t, context.Background(), admin, clocksLockSQL)
		r.seq++
		sub := r.async(http.MethodPost, "/v1/subscriptions", map[string]any{
			"code": "pp-c3w", "display_name": "C3", "customer_id": cust, "billing_time": "anniversary",
			"items": []map[string]any{{"plan_id": plan, "quantity": 1}},
		})
		testutil.WaitForLockWaiter(t, context.Background(), admin, "FOR KEY SHARE")
		h.release()
		e := wait(t, "edit", edit)
		if e.code != http.StatusOK || flowStr(e.body, "effective") != "now" {
			t.Fatalf("edit: %d %s, want 200 effective now", e.code, e.raw)
		}
		s := wait(t, "subscribe", sub)
		if s.code != http.StatusCreated {
			t.Fatalf("subscribe: %d %s", s.code, s.raw)
		}
		subID, p0, p1 := flowStr(s.body, "id"), ppTime(t, flowStr(s.body, "current_billing_period_start")), ppTime(t, flowStr(s.body, "current_billing_period_end"))
		r.ingest(time.Time{}, "pp-c3w", "calls", 10, recent(p0), nil)
		r.run(p1.Add(time.Hour))
		wantUsage(t, "first period", r.cycle(subID, p0), map[string]int64{ids[0]: 50})
	})
	t.Run("the subscription first", func(t *testing.T) {
		r := newPPRig(t, "c3 sub first")
		admin := testutil.AdminPool(t)
		calls := r.meter("calls", "sum")
		// An in_advance base fee: the subscription numbers its first invoice
		// after it locks the plan, so it waits at the number row holding it.
		out := r.must(time.Time{}, http.MethodPost, "/v1/plans", map[string]any{
			"code": "pro", "name": "pro", "currency": "USD", "billing_interval": "monthly",
			"base_amount_cents": 100, "base_bill_timing": "in_advance", "prices": []map[string]any{perUnit(calls, "1", nil)},
		}, http.StatusCreated)
		plan, priceID := flowStr(out, "id"), flowStr(flowList(out, "prices")[0], "id")
		cust := r.customer("pp-c3s", "")
		r.exec(`INSERT INTO tenant_settings (tenant_id) VALUES ($1) ON CONFLICT (tenant_id) DO NOTHING`, r.tenantID)
		h := hold(t, admin, `SELECT 1 FROM tenant_settings WHERE tenant_id = $1 FOR UPDATE`, r.tenantID)
		sub := r.async(http.MethodPost, "/v1/subscriptions", map[string]any{
			"code": "pp-c3s", "display_name": "C3", "customer_id": cust, "billing_time": "anniversary",
			"items": []map[string]any{{"plan_id": plan, "quantity": 1}},
		})
		testutil.WaitForLockWaiter(t, context.Background(), admin, numberRowLockSQL)
		edit := r.async(http.MethodPatch, "/v1/plans/"+plan+"/prices/"+priceID, map[string]any{"mode": "per_unit", "unit_amount_cents": "5"})
		testutil.WaitForLockWaiter(t, context.Background(), admin, planLockSQL)
		h.release()
		s := wait(t, "subscribe", sub)
		if s.code != http.StatusCreated {
			t.Fatalf("subscribe: %d %s", s.code, s.raw)
		}
		e := wait(t, "edit", edit)
		if e.code != http.StatusOK || flowStr(e.body, "effective") != "next_period" {
			t.Fatalf("edit: %d %s, want 200 effective next_period", e.code, e.raw)
		}
		subID, p0, p1 := flowStr(s.body, "id"), ppTime(t, flowStr(s.body, "current_billing_period_start")), ppTime(t, flowStr(s.body, "current_billing_period_end"))
		r.ingest(time.Time{}, "pp-c3s", "calls", 10, recent(p0), nil)
		r.run(p1.Add(time.Hour))
		var usage int64
		for _, i := range r.invoices(subID) {
			if i.reason == "subscription_cycle" {
				usage += i.usageCents()
			}
		}
		if usage != 10 {
			t.Errorf("first period usage = %dc, want 10 (the content the period started with)", usage)
		}
	})
}

// TestE2E_PlanPrices_PriceWriteAgainstClockAdvance (C9): a price write reads
// each test clock FOR SHARE, so it waits for an advance in progress and
// stamps the clock's new time; no period the advance's catch-up closes is
// repriced.
//
// Mutation: read the clocks without FOR SHARE, and the stamp is the old
// time.
func TestE2E_PlanPrices_PriceWriteAgainstClockAdvance(t *testing.T) {
	r := newPPRig(t, "c9")
	admin := testutil.AdminPool(t)
	calls := r.meter("calls", "sum")
	plan, ids := r.plan("pro", perUnit(calls, "1", nil))
	frozen := time.Now().UTC().AddDate(0, 0, -50).Truncate(time.Second)
	clk := r.clock("c9", frozen)
	r.clockSub("pp-c9", clk, plan)
	moved := frozen.Add(36 * time.Hour)
	// The advance in progress: the clock row locked and its time moved,
	// not committed yet.
	h := hold(t, admin, `UPDATE test_clocks SET frozen_time = $2 WHERE id = $1`, clk, moved)
	edit := r.async(http.MethodPatch, "/v1/plans/"+plan+"/prices/"+ids[0], map[string]any{"mode": "per_unit", "unit_amount_cents": "2"})
	testutil.WaitForLockWaiter(t, context.Background(), admin, clocksLockSQL)
	h.commit(t)
	if e := wait(t, "edit", edit); e.code != http.StatusOK {
		t.Fatalf("edit: %d %s", e.code, e.raw)
	}
	var stamp time.Time
	if err := admin.QueryRow(`SELECT valid_from FROM plan_price_version_clocks WHERE test_clock_id = $1`, clk).Scan(&stamp); err != nil {
		t.Fatal(err)
	}
	if want := moved.Add(time.Microsecond); !stamp.Equal(want) {
		t.Errorf("stamp = %v, want the advanced time plus 1µs (%v)", stamp, want)
	}
}

// TestE2E_PlanPrices_SwapAgainstPriceWrites (C10): an immediate swap from
// plan A to plan B against a price write on each, many times. The swap takes
// both plans FOR KEY SHARE in ascending id after its subscription row; a
// price write takes one plan FOR UPDATE, then the test clocks. No cycle: no
// request fails with a deadlock (40P01 answers 500).
func TestE2E_PlanPrices_SwapAgainstPriceWrites(t *testing.T) {
	r := newPPRig(t, "c10")
	calls := r.meter("calls", "sum")
	planA, aIDs := r.plan("plan_a", perUnit(calls, "1", nil))
	planB, bIDs := r.plan("plan_b", perUnit(calls, "2", nil))
	for i := 0; i < 20; i++ {
		cust := r.customer("pp-c10-"+string(rune('a'+i)), "")
		sub, _, _ := r.subscribe(time.Time{}, cust, planA)
		item := flowStr(flowList(r.must(time.Time{}, http.MethodGet, "/v1/subscriptions/"+sub, nil, http.StatusOK), "items")[0], "id")
		swap := r.async(http.MethodPatch, "/v1/subscriptions/"+sub+"/items/"+item, map[string]any{"new_plan_id": planB, "immediate": true})
		ea := r.async(http.MethodPatch, "/v1/plans/"+planA+"/prices/"+aIDs[0], map[string]any{"mode": "per_unit", "unit_amount_cents": "1"})
		eb := r.async(http.MethodPatch, "/v1/plans/"+planB+"/prices/"+bIDs[0], map[string]any{"mode": "per_unit", "unit_amount_cents": "2"})
		for what, ch := range map[string]<-chan ppResp{"swap": swap, "edit A": ea, "edit B": eb} {
			if got := wait(t, what, ch); got.code != http.StatusOK {
				t.Fatalf("run %d %s: %d %s (a 500 is the deadlock in the server log)", i, what, got.code, got.raw)
			}
		}
	}
}

// TestE2E_PlanPrices_TeardownAgainstWrites (round 2): a test-clock teardown
// deletes bottom-up, outside the lock order, so it takes its clock row FOR
// UPDATE first. A price removal or an override write reads the clock FOR
// SHARE before it touches a row teardown deletes (an override of the clock's
// customer), so one waits for the other at the clock and both finish. The
// teardown here stops after it deleted the customer's override, before it
// deletes the customer and the clock.
//
// Mutation: drop the teardown's clock lock, and the removal and the teardown
// deadlock (one answers 500 with 40P01).
func TestE2E_PlanPrices_TeardownAgainstWrites(t *testing.T) {
	for _, writer := range []string{"price removal", "override write"} {
		t.Run(writer, func(t *testing.T) {
			r := newPPRig(t, "teardown "+writer)
			admin := testutil.AdminPool(t)
			calls := r.meter("calls", "sum")
			plan, ids := r.plan("pro", perUnit(calls, "10", nil))
			frozen := time.Now().UTC().AddDate(0, 0, -20).Truncate(time.Second)
			clk := r.clock("td", frozen)
			cust := r.customer("pp-tdw", clk)
			r.subscribe(time.Time{}, cust, plan)
			r.must(time.Time{}, http.MethodPost, "/v1/price-overrides", map[string]any{
				"customer_id": cust, "price_id": ids[0], "mode": "per_unit", "unit_amount_cents": "4",
			}, http.StatusCreated)

			h := hold(t, admin, `SELECT 1 FROM customers WHERE id = $1 FOR UPDATE`, cust)
			teardown := r.async(http.MethodDelete, "/v1/test-clocks/"+clk, nil)
			testutil.WaitForLockWaiter(t, context.Background(), admin, teardownCustSQL)
			var write <-chan ppResp
			if writer == "price removal" {
				write = r.async(http.MethodDelete, "/v1/plans/"+plan+"/prices/"+ids[0], nil)
			} else {
				write = r.async(http.MethodPost, "/v1/price-overrides", map[string]any{
					"customer_id": cust, "price_id": ids[0], "mode": "per_unit", "unit_amount_cents": "3",
				})
			}
			// The write waits at the clock (with the fix), or at the
			// override rows the teardown deleted (without it, and then the
			// two deadlock once the teardown goes on).
			waitForAnyLockWaiter(t, admin, "test_clocks", "customer_price_overrides")
			h.release()
			if got := wait(t, "teardown", teardown); got.code >= 300 {
				t.Fatalf("teardown: %d %s", got.code, got.raw)
			}
			got := wait(t, writer, write)
			switch {
			case writer == "price removal" && got.code == http.StatusOK:
			case writer == "override write" && got.code == http.StatusNotFound:
				// The customer went with the clock.
			default:
				t.Fatalf("%s: %d %s (a 500 is the deadlock in the server log)", writer, got.code, got.raw)
			}
		})
	}
}

// TestE2E_PlanPrices_TeardownSubscriptionWriterAndPriceWrite: three parties.
// A teardown holds its clock FOR UPDATE and waits to delete a subscription a
// subscription writer holds; that writer next takes its plans FOR KEY SHARE;
// a price write on the plan reads the clocks FOR SHARE. The price write
// reads the clocks before it locks the plan (ADR-130 §8), so it waits at the
// clock holding no plan, the writer gets its plan, and all three finish.
//
// Mutation: read the clocks after the plan lock, and the price write holds
// the plan the writer waits for while it waits for the teardown's clock:
// Postgres aborts one of the three with 40P01.
func TestE2E_PlanPrices_TeardownSubscriptionWriterAndPriceWrite(t *testing.T) {
	r := newPPRig(t, "teardown three")
	admin := testutil.AdminPool(t)
	calls := r.meter("calls", "sum")
	plan, ids := r.plan("pro", perUnit(calls, "10", nil))
	clk := r.clock("td3", time.Now().UTC().AddDate(0, 0, -20).Truncate(time.Second))
	sub, _, _ := r.clockSub("pp-td3", clk, plan)

	// The subscription writer: its subscription row first, as every
	// subscription writer does (ADR-115).
	w := hold(t, admin, `SELECT 1 FROM subscriptions WHERE id = $1 FOR NO KEY UPDATE`, sub)
	teardown := r.async(http.MethodDelete, "/v1/test-clocks/"+clk, nil)
	testutil.WaitForLockWaiter(t, context.Background(), admin, "DELETE FROM subscriptions")
	edit := r.async(http.MethodPatch, "/v1/plans/"+plan+"/prices/"+ids[0], map[string]any{"mode": "per_unit", "unit_amount_cents": "11"})
	testutil.WaitForLockWaiter(t, context.Background(), admin, clocksLockSQL)

	// The writer's next step: its plans, as lockPlansTx takes them.
	locked := make(chan error, 1)
	go func() {
		_, err := w.tx.Exec(`SELECT id FROM plans WHERE id = ANY($1) ORDER BY id FOR KEY SHARE`, "{"+plan+"}")
		locked <- err
	}()
	select {
	case err := <-locked:
		if err != nil {
			t.Fatalf("the subscription writer's plan lock: %v (40P01 is the deadlock)", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the subscription writer's plan lock never returned")
	}
	w.release()
	if got := wait(t, "teardown", teardown); got.code >= 300 {
		t.Fatalf("teardown: %d %s (a 500 is the deadlock in the server log)", got.code, got.raw)
	}
	if got := wait(t, "price write", edit); got.code != http.StatusOK {
		t.Fatalf("price write: %d %s (a 500 is the deadlock in the server log)", got.code, got.raw)
	}
}

// waitForAnyLockWaiter returns once a backend waits on a lock in a statement
// matching one of patterns.
func waitForAnyLockWaiter(t *testing.T, admin *sql.DB, patterns ...string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range patterns {
			var n int
			if err := admin.QueryRow(`SELECT count(*) FROM pg_stat_activity
				WHERE datname = current_database() AND wait_event_type = 'Lock' AND query ILIKE $1
				  AND query NOT ILIKE '%DELETE FROM customers%'`, "%"+p+"%").Scan(&n); err != nil {
				t.Fatalf("read pg_stat_activity: %v", err)
			}
			if n > 0 {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no backend waits on a lock in a statement matching %v", patterns)
}

// TestE2E_PlanPrices_OverrideStampAfterItsLocks (round 2): an override write
// stamps its window after its locks, with the database's wall clock, so a
// write that waited for a price write across a period start does not change
// the period already open.
//
// Mutation: stamp with now() (the transaction's start), and the override
// starts before the lock was let go.
func TestE2E_PlanPrices_OverrideStampAfterItsLocks(t *testing.T) {
	r := newPPRig(t, "override stamp")
	admin := testutil.AdminPool(t)
	calls := r.meter("calls", "sum")
	plan, ids := r.plan("pro", perUnit(calls, "10", nil))
	cust := r.customer("pp-os", "")
	h := hold(t, admin, `SELECT 1 FROM plans WHERE id = $1 FOR UPDATE`, plan)
	o := r.async(http.MethodPost, "/v1/price-overrides", map[string]any{
		"customer_id": cust, "price_id": ids[0], "mode": "per_unit", "unit_amount_cents": "4",
	})
	testutil.WaitForLockWaiter(t, context.Background(), admin, "FOR KEY SHARE")
	time.Sleep(50 * time.Millisecond)
	var released time.Time
	if err := admin.QueryRow(`SELECT clock_timestamp()`).Scan(&released); err != nil {
		t.Fatal(err)
	}
	h.release()
	got := wait(t, "override", o)
	if got.code != http.StatusCreated {
		t.Fatalf("override: %d %s", got.code, got.raw)
	}
	if created := ppTime(t, flowStr(got.body, "created_at")); created.Before(released) {
		t.Errorf("override starts %v, before its plan lock was let go at %v", created, released)
	}
}

// TestE2E_PlanPrices_RemovalEndsOverridesAtTheTombstone (round 2): a removal
// ends each open override at the instant the removal takes effect for its
// customer: the tombstone's valid_from for a customer without a clock, the
// tombstone's stamp for a clock customer's clock.
//
// Mutation: end them at the request's start, or 1µs before the tombstone,
// and the deactivated_at values differ.
func TestE2E_PlanPrices_RemovalEndsOverridesAtTheTombstone(t *testing.T) {
	r := newPPRig(t, "removal instants")
	calls := r.meter("calls", "sum")
	plan, ids := r.plan("pro", perUnit(calls, "10", nil))
	wallCust := r.customer("pp-rw", "")
	r.subscribe(time.Time{}, wallCust, plan)
	frozen := time.Now().UTC().AddDate(0, 0, 30).Truncate(time.Second)
	clk := r.clock("ri", frozen)
	clockCust := r.customer("pp-rc", clk)
	for _, c := range []string{wallCust, clockCust} {
		r.must(time.Time{}, http.MethodPost, "/v1/price-overrides", map[string]any{
			"customer_id": c, "price_id": ids[0], "mode": "per_unit", "unit_amount_cents": "4",
		}, http.StatusCreated)
	}
	r.must(time.Time{}, http.MethodDelete, "/v1/plans/"+plan+"/prices/"+ids[0], nil, http.StatusOK)
	ctx := context.Background()
	admin := testutil.AdminPool(t)
	var tombFrom, tombStamp time.Time
	if err := admin.QueryRowContext(ctx, `
		SELECT v.valid_from, c.valid_from FROM plan_price_versions v
		JOIN plan_price_version_clocks c ON c.version_id = v.id AND c.test_clock_id = $2
		WHERE v.price_id = $1 AND v.removed`, ids[0], clk).Scan(&tombFrom, &tombStamp); err != nil {
		t.Fatalf("read the tombstone: %v", err)
	}
	for cust, want := range map[string]time.Time{wallCust: tombFrom, clockCust: tombStamp} {
		var ended time.Time
		if err := admin.QueryRowContext(ctx, `SELECT deactivated_at FROM customer_price_overrides WHERE customer_id = $1`, cust).Scan(&ended); err != nil {
			t.Fatal(err)
		}
		if !ended.Equal(want) {
			t.Errorf("override of %s ended at %v, want %v", cust, ended, want)
		}
	}
}
