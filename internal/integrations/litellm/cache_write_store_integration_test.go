package litellm

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// The replay guard for the cache-write split, on real Postgres.
//
// Before the split, a call with cache writes was stored as <id>:input with
// quantity prompt − cache_read, the writes inside it. After the split the
// same call maps to a smaller <id>:input plus <id>:cache_write_5m and
// <id>:cache_write_1h. A LiteLLM resend (or an operator replay of spend
// logs) of a call first stored the old way would dedup :input on its key
// and add the write rows on new keys: the writes billed twice. The store
// therefore decides per call, in the transaction that writes it, from the
// :input row already stored (in usage_events or usage_held_events, any
// status): if it carries the old quantity, the call is written the old way.
//
// Every test sends the probe call (1,000 fresh input, 40,000 5m writes,
// 60,000 1h writes, 500 output) and, as a control, a fresh call that must
// split, so a mapper that never splits cannot pass.

type writeFixture struct {
	t        *testing.T
	db       *postgres.DB
	ctx      context.Context
	tenantID string
	svc      *usage.Service
	h        *Handler
}

func newWriteFixture(t *testing.T, name string) *writeFixture {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, name)
	svc := usage.NewService(usage.NewPostgresStore(db))
	return &writeFixture{t: t, db: db, ctx: ctx, tenantID: tenantID, svc: svc,
		h: New(customer.NewPostgresStore(db), pricing.NewService(pricing.NewPostgresStore(db)), svc, svc)}
}

// probeCall is the split fixture for one customer under a chosen id.
func (f *writeFixture) probeCall(id, user string) StandardLoggingPayload {
	p := mockAnthropicRecord(f.t, "cache-write-5m-1h")
	p.ID, p.EndUser = id, user
	return p
}

func (f *writeFixture) spend(p StandardLoggingPayload) (int, SpendResponse) {
	f.t.Helper()
	code, body := postSpendWithCtx(f.t, f.h, f.ctx, f.tenantID, p)
	var r SpendResponse
	if code == http.StatusOK {
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			f.t.Fatalf("decode %s: %v", body, err)
		}
	}
	return code, r
}

func (f *writeFixture) mustSpend(p StandardLoggingPayload) SpendResponse {
	f.t.Helper()
	code, r := f.spend(p)
	if code != http.StatusOK {
		f.t.Fatalf("spend %s: status %d", p.ID, code)
	}
	return r
}

// customerAndMeter creates the customer with this external id and the
// tokens meter, returning their ids.
func (f *writeFixture) customerAndMeter(externalID string) (string, string) {
	f.t.Helper()
	tx, err := f.db.BeginTx(f.ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		f.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	customerID := postgres.NewID("vlx_cus")
	if _, err := tx.ExecContext(f.ctx, `INSERT INTO customers (id, tenant_id, external_id, display_name, email) VALUES ($1, $2, $3, 'Writes', 'writes@example.com')`, customerID, f.tenantID, externalID); err != nil {
		f.t.Fatalf("customer: %v", err)
	}
	var meterID string
	if err := tx.QueryRowContext(f.ctx, `SELECT id FROM meters WHERE key = 'tokens'`).Scan(&meterID); err != nil {
		meterID = postgres.NewID("vlx_mtr")
		if _, err := tx.ExecContext(f.ctx, `INSERT INTO meters (id, tenant_id, name, key, unit, aggregation) VALUES ($1, $2, 'Tokens', 'tokens', 'tokens', 'sum')`, meterID, f.tenantID); err != nil {
			f.t.Fatalf("meter: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		f.t.Fatalf("commit: %v", err)
	}
	return customerID, meterID
}

// rows returns the quantity stored per token type for one call id, in
// usage_events, or in usage_held_events with the given status.
func (f *writeFixture) rows(table, id, heldStatus string) map[string]int64 {
	f.t.Helper()
	tx, err := f.db.BeginTx(f.ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		f.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	q := `SELECT idempotency_key, quantity FROM usage_events WHERE idempotency_key LIKE $1 || ':%'`
	args := []any{id}
	if table == "held" {
		q = `SELECT idempotency_key, quantity FROM usage_held_events WHERE idempotency_key LIKE $1 || ':%' AND status = $2`
		args = append(args, heldStatus)
	}
	rs, err := tx.QueryContext(f.ctx, q, args...)
	if err != nil {
		f.t.Fatalf("query: %v", err)
	}
	defer func() { _ = rs.Close() }()
	out := map[string]int64{}
	for rs.Next() {
		var key string
		var qty decimal.Decimal
		if err := rs.Scan(&key, &qty); err != nil {
			f.t.Fatalf("scan: %v", err)
		}
		out[key[len(id)+1:]] = qty.IntPart()
	}
	if err := rs.Err(); err != nil {
		f.t.Fatalf("rows: %v", err)
	}
	return out
}

var (
	splitRows  = map[string]int64{TokenTypeInput: 1000, TokenTypeCacheWrite5m: 40000, TokenTypeCacheWrite1h: 60000, TokenTypeOutput: 500}
	legacyRows = map[string]int64{TokenTypeInput: 101000, TokenTypeOutput: 500}
)

func (f *writeFixture) expectRows(what, table, id, heldStatus string, want map[string]int64) {
	f.t.Helper()
	if got := f.rows(table, id, heldStatus); !sameQuantities(got, want) {
		f.t.Errorf("%s: %s rows for %s = %v, want %v", what, table, id, got, want)
	}
}

// ingestLegacy stores a call the way the mapper before the split did, one
// event at a time, as a copy of the previous release still does.
func (f *writeFixture) ingestLegacy(id, customerID, meterID string) {
	f.t.Helper()
	for tt, q := range legacyRows {
		if _, err := f.svc.IngestResolvingHeld(f.ctx, f.tenantID, usage.IngestInput{
			CustomerID: customerID, MeterID: meterID, Quantity: decimal.NewFromInt(q),
			Dimensions:     map[string]any{"model": "claude-sonnet-4.5", "provider": "anthropic", "token_type": tt},
			IdempotencyKey: id + ":" + tt,
		}); err != nil {
			f.t.Fatalf("legacy ingest %s: %v", tt, err)
		}
	}
}

func (f *writeFixture) holdLegacy(id, user string) {
	f.t.Helper()
	for tt, q := range legacyRows {
		if _, err := f.svc.Hold(f.ctx, f.tenantID, usage.HoldInput{
			ExternalCustomerID: user, MeterKey: MeterKeyTokens, Quantity: decimal.NewFromInt(q),
			Dimensions:     map[string]any{"model": "claude-sonnet-4.5", "provider": "anthropic", "token_type": tt},
			IdempotencyKey: id + ":" + tt, Reason: domain.HeldReasonCustomerNotFound,
		}); err != nil {
			f.t.Fatalf("legacy hold %s: %v", tt, err)
		}
	}
}

// A call stored before the fix, resent after it, gets no write rows: its
// :input already bills the writes.
// Mutation-verify: remove the guard → the resend adds both write rows.
func TestSpend_PreFixInputGetsNoWriteRows_RealStore(t *testing.T) {
	f := newWriteFixture(t, "LiteLLM writes prefix")
	cus, mtr := f.customerAndMeter("cus_w")
	f.ingestLegacy("old-1", cus, mtr)

	r := f.mustSpend(f.probeCall("old-1", "cus_w"))
	if r.Accepted != 0 || r.Deduplicated != 2 {
		t.Errorf("resend of a pre-fix call = %+v, want 0 accepted and 2 deduplicated", r)
	}
	f.expectRows("pre-fix call resent", "events", "old-1", "", legacyRows)

	if r := f.mustSpend(f.probeCall("new-1", "cus_w")); r.Accepted != 4 {
		t.Errorf("control call = %+v, want 4 accepted", r)
	}
	f.expectRows("control", "events", "new-1", "", splitRows)
}

// The same through the held table. A pre-fix call held for a customer that
// did not exist yet:
//   - resent while the customer is still missing: nothing new is held;
//   - resent once the customer exists: :input is written at the OLD
//     quantity, the held rows are released, and no write rows are written;
//   - discarded by the operator, then resent once the customer exists:
//     nothing is billed.
//
// Mutation-verify: the guard reads usage_events only → the first two hold
// or write the write rows; the old-way mapping keeps the new :input
// quantity → :input is written as 1000 and the writes bill nowhere.
func TestSpend_PreFixHeldCall_RealStore(t *testing.T) {
	t.Run("customer still missing", func(t *testing.T) {
		f := newWriteFixture(t, "LiteLLM writes held missing")
		f.holdLegacy("old-h1", "cus_late")
		r := f.mustSpend(f.probeCall("old-h1", "cus_late"))
		if r.Held != 0 || r.Deduplicated != 2 {
			t.Errorf("resend = %+v, want 0 held and 2 deduplicated", r)
		}
		f.expectRows("pre-fix held, resent", "held", "old-h1", "held", legacyRows)

		if r := f.mustSpend(f.probeCall("new-h1", "cus_late")); r.Held != 4 {
			t.Errorf("control = %+v, want 4 held", r)
		}
		f.expectRows("control", "held", "new-h1", "held", splitRows)
	})

	t.Run("customer now exists", func(t *testing.T) {
		f := newWriteFixture(t, "LiteLLM writes held exists")
		f.holdLegacy("old-h2", "cus_late")
		f.customerAndMeter("cus_late")
		r := f.mustSpend(f.probeCall("old-h2", "cus_late"))
		if r.Accepted != 2 {
			t.Errorf("resend = %+v, want 2 accepted (the old input and output)", r)
		}
		f.expectRows("pre-fix held, resent with the customer", "events", "old-h2", "", legacyRows)
		f.expectRows("held rows released", "held", "old-h2", "released", legacyRows)
	})

	t.Run("discarded", func(t *testing.T) {
		f := newWriteFixture(t, "LiteLLM writes held discarded")
		f.holdLegacy("old-h3", "cus_gone")
		if n, err := f.svc.Discard(f.ctx, f.tenantID, "cus_gone", ""); err != nil || n != 2 {
			t.Fatalf("discard = %d (%v), want 2", n, err)
		}
		f.customerAndMeter("cus_gone")
		r := f.mustSpend(f.probeCall("old-h3", "cus_gone"))
		if r.Accepted != 0 || r.Deduplicated != 2 {
			t.Errorf("resend after discard = %+v, want 2 deduplicated", r)
		}
		f.expectRows("discarded call resent", "events", "old-h3", "", map[string]int64{})
	})
}

// A call held after the fix keeps its split: a resend once the customer
// exists writes all four rows and releases them, and an operator release
// writes them once.
func TestSpend_PostFixHeldCall_RealStore(t *testing.T) {
	t.Run("resent", func(t *testing.T) {
		f := newWriteFixture(t, "LiteLLM writes postfix resend")
		if r := f.mustSpend(f.probeCall("pf-1", "cus_pf")); r.Held != 4 {
			t.Fatalf("first delivery = %+v, want 4 held", r)
		}
		f.customerAndMeter("cus_pf")
		if r := f.mustSpend(f.probeCall("pf-1", "cus_pf")); r.Accepted != 4 {
			t.Errorf("resend with the customer = %+v, want 4 accepted", r)
		}
		f.expectRows("post-fix held, resent", "events", "pf-1", "", splitRows)
		f.expectRows("held rows released", "held", "pf-1", "released", splitRows)
	})

	t.Run("released by the operator", func(t *testing.T) {
		f := newWriteFixture(t, "LiteLLM writes postfix release")
		f.mustSpend(f.probeCall("pf-2", "cus_pf2"))
		cus, mtr := f.customerAndMeter("cus_pf2")
		res, err := f.svc.Release(f.ctx, f.tenantID, usage.ReleaseInput{
			ExternalCustomerID: "cus_pf2", CustomerID: cus, MeterIDs: map[string]string{MeterKeyTokens: mtr},
		})
		if err != nil || res.Released != 4 {
			t.Fatalf("release = %+v (%v), want 4 released", res, err)
		}
		if r := f.mustSpend(f.probeCall("pf-2", "cus_pf2")); r.Accepted != 0 || r.Deduplicated != 4 {
			t.Errorf("resend after release = %+v, want 4 deduplicated", r)
		}
		f.expectRows("post-fix held, released", "events", "pf-2", "", splitRows)
	})
}

// A call is one transaction. A failure on its last insert (here a trigger
// that refuses the 1-hour write row) leaves nothing of it: the batch is
// refused with 503, LiteLLM resends it, and the resend stores all of it.
// With one transaction per event the input and 5-minute rows would commit
// first, and the guard would then read that :input on the resend.
// Mutation-verify: one transaction per event → input and cache_write_5m
// rows survive the failure.
func TestSpend_CallIsOneTransaction_RealStore(t *testing.T) {
	f := newWriteFixture(t, "LiteLLM writes fault")
	f.customerAndMeter("cus_fault")
	admin := testutil.AdminPool(t)
	if _, err := admin.ExecContext(f.ctx, `
		CREATE OR REPLACE FUNCTION test_refuse_fault_write() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected fault on %', NEW.idempotency_key; END $$;
		CREATE TRIGGER test_refuse_fault_write BEFORE INSERT ON usage_events
		FOR EACH ROW WHEN (NEW.idempotency_key = 'fault-1:cache_write_1h')
		EXECUTE FUNCTION test_refuse_fault_write();
	`); err != nil {
		t.Fatalf("install fault: %v", err)
	}
	dropped := false
	drop := func() {
		if dropped {
			return
		}
		dropped = true
		if _, err := admin.ExecContext(context.Background(), `
			DROP TRIGGER IF EXISTS test_refuse_fault_write ON usage_events;
			DROP FUNCTION IF EXISTS test_refuse_fault_write();`); err != nil {
			t.Errorf("drop fault: %v", err)
		}
	}
	t.Cleanup(drop)

	if code, r := f.spend(f.probeCall("fault-1", "cus_fault")); code != http.StatusServiceUnavailable {
		t.Fatalf("faulted call: status %d (%+v), want 503", code, r)
	}
	f.expectRows("after the fault", "events", "fault-1", "", map[string]int64{})

	drop()
	if r := f.mustSpend(f.probeCall("fault-1", "cus_fault")); r.Accepted != 4 {
		t.Errorf("retry = %+v, want 4 accepted", r)
	}
	f.expectRows("after the retry", "events", "fault-1", "", splitRows)
}

// Two deliveries of one call at once (a LiteLLM timeout retry while the
// first request is still running, on another replica) store each row once.
func TestSpend_ConcurrentDeliveries_RealStore(t *testing.T) {
	f := newWriteFixture(t, "LiteLLM writes concurrent")
	f.customerAndMeter("cus_cc")
	p := f.probeCall("cc-1", "cus_cc")
	var wg sync.WaitGroup
	results := make([]SpendResponse, 2)
	codes := make([]int, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, body := postSpendWithCtx(t, f.h, f.ctx, f.tenantID, p)
			codes[i] = code
			_ = json.Unmarshal([]byte(body), &results[i])
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("delivery %d: status %d", i, c)
		}
	}
	if a, d := results[0].Accepted+results[1].Accepted, results[0].Deduplicated+results[1].Deduplicated; a != 4 || d != 4 {
		t.Errorf("accepted %d deduplicated %d across both, want 4 and 4 (%+v)", a, d, results)
	}
	f.expectRows("two deliveries", "events", "cc-1", "", splitRows)
}

// Rolling deploy: a copy of the previous release is writing the old :input
// for a call (its transaction not yet committed) while this release handles
// a resend of the same call. The new path's claim insert on :input waits
// for the old transaction, then sees the old row and writes the call the
// old way: no write rows on top of an :input that already bills them.
// Mutation-verify: check for :input with a SELECT before inserting (the
// old row is not visible yet) → the write rows are written: double billing.
func TestSpend_OldBinaryRace_RealStore(t *testing.T) {
	f := newWriteFixture(t, "LiteLLM writes rolling deploy")
	cus, mtr := f.customerAndMeter("cus_rd")
	admin := testutil.AdminPool(t)

	old, err := f.db.BeginTx(f.ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(old)
	if _, err := old.ExecContext(f.ctx, `
		INSERT INTO usage_events (id, tenant_id, customer_id, meter_id, quantity, properties, idempotency_key, timestamp, origin)
		VALUES ($1, $2, $3, $4, 101000, '{"model":"claude-sonnet-4.5","provider":"anthropic","token_type":"input"}', 'rd-1:input', now(), 'api')
	`, postgres.NewID("vlx_evt"), f.tenantID, cus, mtr); err != nil {
		t.Fatalf("old-binary insert: %v", err)
	}

	done := make(chan SpendResponse, 1)
	go func() {
		code, body := postSpendWithCtx(t, f.h, f.ctx, f.tenantID, f.probeCall("rd-1", "cus_rd"))
		var r SpendResponse
		if code == http.StatusOK {
			_ = json.Unmarshal([]byte(body), &r)
		}
		done <- r
	}()
	testutil.WaitForLockWaiter(t, f.ctx, admin, "provider_cost_rates")
	if err := old.Commit(); err != nil {
		t.Fatalf("old-binary commit: %v", err)
	}
	select {
	case r := <-done:
		if r.Accepted != 1 {
			t.Errorf("new path = %+v, want only the output row accepted", r)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("new path never finished")
	}
	f.expectRows("old and new binary on one call", "events", "rd-1", "", legacyRows)

	if r := f.mustSpend(f.probeCall("rd-2", "cus_rd")); r.Accepted != 4 {
		t.Errorf("control = %+v, want 4 accepted", r)
	}
}

// A LiteLLM response-cache hit stores nothing, held or not.
func TestSpend_CacheHitStoresNothing_RealStore(t *testing.T) {
	f := newWriteFixture(t, "LiteLLM cache hit")
	f.customerAndMeter("cus_hit")
	p := mockAnthropicRecord(t, "cache-hit")
	p.EndUser = "cus_hit"
	if r := f.mustSpend(p); r.Skipped != 1 || r.CacheHits != 1 || r.Accepted != 0 {
		t.Errorf("cache hit = %+v, want skipped 1 and cache_hits 1", r)
	}
	f.expectRows("cache hit", "events", p.ID, "", map[string]int64{})
}

// A call held split by this release, then resent through a copy of the
// previous release once the customer exists (a rolling-deploy overlap, a
// binary rollback, or a spend-log replay served by an old copy). The old
// copy writes :input at the pre-split quantity, which bills the writes,
// and releases the held :input and :output. The two held write rows must
// then never be billed: not by an operator release, not by a resend.
//
// Mutation-verify:
//   - ReleaseHeld writes a held write row without checking the call's
//     stored :input → the release bills 100,000 write tokens twice;
//   - IngestCallResolvingHeld lets the held :input's quantity decide over
//     the stored :input → the resend bills them twice;
//   - the Legacy path leaves the call's held write rows held → they stay
//     on the held list.
func TestSpend_HeldSplitThenOldBinaryIngest_RealStore(t *testing.T) {
	setup := func(t *testing.T, name, id, user string) (*writeFixture, string, string) {
		f := newWriteFixture(t, name)
		if r := f.mustSpend(f.probeCall(id, user)); r.Held != 4 {
			t.Fatalf("first delivery = %+v, want 4 held", r)
		}
		cus, mtr := f.customerAndMeter(user)
		f.ingestLegacy(id, cus, mtr)
		f.expectRows("old copy wrote the call", "events", id, "", legacyRows)
		f.expectRows("old copy released input and output", "held", id, "released", legacyHeldInputOutput)
		return f, cus, mtr
	}

	t.Run("operator release", func(t *testing.T) {
		f, cus, mtr := setup(t, "LiteLLM split then old release", "mix-1", "cus_mix1")
		res, err := f.svc.Release(f.ctx, f.tenantID, usage.ReleaseInput{
			ExternalCustomerID: "cus_mix1", CustomerID: cus, MeterIDs: map[string]string{MeterKeyTokens: mtr},
		})
		if err != nil {
			t.Fatalf("release: %v", err)
		}
		if res.Released != 0 || res.Deduplicated != 2 {
			t.Errorf("release = %+v, want 0 released and 2 deduplicated", res)
		}
		f.expectRows("after the release", "events", "mix-1", "", legacyRows)
		f.expectRows("nothing left held", "held", "mix-1", "held", map[string]int64{})
	})

	t.Run("resend", func(t *testing.T) {
		f, _, _ := setup(t, "LiteLLM split then old resend", "mix-2", "cus_mix2")
		if r := f.mustSpend(f.probeCall("mix-2", "cus_mix2")); r.Accepted != 0 || r.Deduplicated != 2 {
			t.Errorf("resend = %+v, want 0 accepted and 2 deduplicated", r)
		}
		f.expectRows("after the resend", "events", "mix-2", "", legacyRows)
		f.expectRows("nothing left held", "held", "mix-2", "held", map[string]int64{})
	})
}

// legacyHeldInputOutput is the held rows a pre-split ingest releases for a
// call held split: its :input (held at the split quantity) and :output.
var legacyHeldInputOutput = map[string]int64{TokenTypeInput: 1000, TokenTypeOutput: 500}

// A call stored before the fix, resent when its customer lookup misses (the
// external id changed, the customer was recreated): the hold reads the
// stored :input and holds the call the pre-split way, so the held list
// shows no write rows and a release bills none.
// Mutation-verify: drop HoldCall's usage_events lookup → the split is held,
// write rows included.
func TestSpend_PreFixCallHeldOnLookupMiss_RealStore(t *testing.T) {
	f := newWriteFixture(t, "LiteLLM prefix held on miss")
	cus, mtr := f.customerAndMeter("cus_before")
	f.ingestLegacy("mv-1", cus, mtr)

	if r := f.mustSpend(f.probeCall("mv-1", "cus_after")); r.Held != 2 {
		t.Errorf("resend under a new external id = %+v, want 2 held (the pre-split rows)", r)
	}
	f.expectRows("held on the lookup miss", "held", "mv-1", "held", legacyRows)

	cus2, _ := f.customerAndMeter("cus_after")
	res, err := f.svc.Release(f.ctx, f.tenantID, usage.ReleaseInput{
		ExternalCustomerID: "cus_after", CustomerID: cus2, MeterIDs: map[string]string{MeterKeyTokens: mtr},
	})
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if res.Released != 0 || res.Deduplicated != 2 {
		t.Errorf("release = %+v, want 0 released and 2 deduplicated", res)
	}
	f.expectRows("after the release", "events", "mv-1", "", legacyRows)
}

// An operator discard and a resend of the same held call at once. The
// resend reads the held rows under their lock, so it waits for the discard
// and then sees it: nothing is billed. Read without the lock, the resend
// would see "held", write the usage, and its release UPDATE would then
// match no row, so nothing would report the discarded usage being billed.
// Mutation-verify: drop FOR UPDATE in lockHeldKeys → 4 rows billed.
func TestSpend_DiscardRacesResend_RealStore(t *testing.T) {
	f := newWriteFixture(t, "LiteLLM discard race")
	if r := f.mustSpend(f.probeCall("dr-1", "cus_dr")); r.Held != 4 {
		t.Fatalf("first delivery = %+v, want 4 held", r)
	}
	f.customerAndMeter("cus_dr")
	admin := testutil.AdminPool(t)
	store := usage.NewPostgresStore(f.db)

	inDiscard, letGo := make(chan struct{}), make(chan struct{})
	discarded := make(chan error, 1)
	go func() {
		_, err := store.DiscardHeld(f.ctx, f.tenantID, "cus_dr", "", func(*sql.Tx, int) error {
			close(inDiscard)
			<-letGo
			return nil
		})
		discarded <- err
	}()
	<-inDiscard

	done := make(chan SpendResponse, 1)
	go func() {
		code, body := postSpendWithCtx(t, f.h, f.ctx, f.tenantID, f.probeCall("dr-1", "cus_dr"))
		var r SpendResponse
		if code == http.StatusOK {
			_ = json.Unmarshal([]byte(body), &r)
		}
		done <- r
	}()
	testutil.WaitForLockWaiter(t, f.ctx, admin, "usage_held_events")
	close(letGo)
	if err := <-discarded; err != nil {
		t.Fatalf("discard: %v", err)
	}
	select {
	case r := <-done:
		if r.Accepted != 0 || r.Deduplicated != 4 {
			t.Errorf("resend = %+v, want 0 accepted and 4 deduplicated", r)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("resend never finished")
	}
	f.expectRows("discarded call", "events", "dr-1", "", map[string]int64{})
	f.expectRows("all rows discarded", "held", "dr-1", "discarded", splitRows)
}

// A resend that deadlocks with another transaction on the call's held rows
// (an operator release claims them in no particular order) is retried once
// inside the store, so LiteLLM gets 200 instead of a 503. The test builds
// the deadlock exactly: it holds the :output row, lets the resend lock the
// rows before it and queue on :output, then asks for :cache_write_1h. The
// resend's deadlock check fires first (the test's session waits 10s before
// checking), so Postgres aborts the resend.
// Mutation-verify: no retry → 503.
func TestSpend_DeadlockedResendRetried_RealStore(t *testing.T) {
	f := newWriteFixture(t, "LiteLLM deadlock retry")
	if r := f.mustSpend(f.probeCall("dl-1", "cus_dl")); r.Held != 4 {
		t.Fatalf("first delivery = %+v, want 4 held", r)
	}
	f.customerAndMeter("cus_dl")
	admin := testutil.AdminPool(t)

	other, err := admin.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = other.Rollback() }()
	if _, err := other.ExecContext(f.ctx, `SET LOCAL deadlock_timeout = '10s'`); err != nil {
		t.Fatalf("deadlock_timeout: %v", err)
	}
	lockRow := func(key string) error {
		_, err := other.ExecContext(f.ctx, `SELECT 1 FROM usage_held_events WHERE idempotency_key = $1 FOR UPDATE`, key)
		return err
	}
	if err := lockRow("dl-1:output"); err != nil {
		t.Fatalf("lock output: %v", err)
	}

	type outcome struct {
		code int
		r    SpendResponse
	}
	done := make(chan outcome, 1)
	go func() {
		code, body := postSpendWithCtx(t, f.h, f.ctx, f.tenantID, f.probeCall("dl-1", "cus_dl"))
		var r SpendResponse
		if code == http.StatusOK {
			_ = json.Unmarshal([]byte(body), &r)
		}
		done <- outcome{code, r}
	}()
	testutil.WaitForLockWaiter(t, f.ctx, admin, "idempotency_key, id, status, quantity")
	// Closes the cycle. Postgres aborts the resend, which frees this lock.
	if err := lockRow("dl-1:cache_write_1h"); err != nil {
		t.Fatalf("the test's own lock was chosen as the deadlock victim (the interleaving was not reached): %v", err)
	}
	if err := other.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	select {
	case o := <-done:
		if o.code != http.StatusOK || o.r.Accepted != 4 {
			t.Errorf("deadlocked resend = %d %+v, want 200 with 4 accepted", o.code, o.r)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("resend never finished")
	}
	f.expectRows("after the retry", "events", "dl-1", "", splitRows)
}

// A release that takes a call's held write rows but not its held :input
// (cut by Limit), while a copy of the previous release is writing the old
// :input for the same call (its transaction not committed yet). The release
// reads the call's :input under the held :input's lock, so it waits, then
// sees the old :input and bills no write row. The held rows are inserted
// with ids that put :input last, so the cut is certain.
// Mutation-verify: drop FOR UPDATE in writesBilledInInput → both write rows
// are written on top of the old :input.
func TestRelease_WaitsForInFlightOldInput_RealStore(t *testing.T) {
	f := newWriteFixture(t, "LiteLLM release vs old input")
	cus, mtr := f.customerAndMeter("cus_cut")
	admin := testutil.AdminPool(t)
	hold, err := f.db.BeginTx(f.ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(hold)
	for i, r := range []struct {
		role string
		qty  int64
	}{{TokenTypeCacheWrite5m, 40000}, {TokenTypeCacheWrite1h, 60000}, {TokenTypeOutput, 500}, {TokenTypeInput, 1000}} {
		if _, err := hold.ExecContext(f.ctx, `
			INSERT INTO usage_held_events (id, tenant_id, source, external_customer_id, meter_key,
				quantity, properties, idempotency_key, event_at, reason)
			VALUES ($1, $2, 'litellm', 'cus_cut', 'tokens', $3, jsonb_build_object('token_type', $4::text),
				'cut-1:' || $4, '2026-10-01T00:00:00Z', 'customer_not_found')
		`, fmt.Sprintf("vlx_hue_cut%d", i), f.tenantID, r.qty, r.role); err != nil {
			t.Fatalf("hold %s: %v", r.role, err)
		}
	}
	if err := hold.Commit(); err != nil {
		t.Fatalf("hold commit: %v", err)
	}

	// The previous release's IngestResolvingHeld for :input, mid-flight: it
	// has locked the held :input and written the old :input.
	old, err := f.db.BeginTx(f.ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(old)
	if _, err := old.ExecContext(f.ctx, `SELECT 1 FROM usage_held_events WHERE idempotency_key = 'cut-1:input' FOR UPDATE`); err != nil {
		t.Fatalf("old lock: %v", err)
	}
	if _, err := old.ExecContext(f.ctx, `
		INSERT INTO usage_events (id, tenant_id, customer_id, meter_id, quantity, properties, idempotency_key, timestamp, origin)
		VALUES ($1, $2, $3, $4, 101000, '{"token_type":"input"}', 'cut-1:input', '2026-10-01T00:00:00Z', 'api')
	`, postgres.NewID("vlx_evt"), f.tenantID, cus, mtr); err != nil {
		t.Fatalf("old input: %v", err)
	}
	if _, err := old.ExecContext(f.ctx, `UPDATE usage_held_events SET status = 'released', resolved_at = now() WHERE idempotency_key = 'cut-1:input'`); err != nil {
		t.Fatalf("old release: %v", err)
	}

	store := usage.NewPostgresStore(f.db)
	type outcome struct {
		res usage.ReleaseHeldResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := store.ReleaseHeld(f.ctx, f.tenantID, usage.ReleaseHeldInput{
			ExternalCustomerID: "cus_cut", CustomerID: cus, MeterIDs: map[string]string{MeterKeyTokens: mtr},
			NotAfter: time.Now(), Limit: 3,
		}, nil)
		done <- outcome{res, err}
	}()
	testutil.WaitForLockWaiter(t, f.ctx, admin, "usage_held_events")
	if err := old.Commit(); err != nil {
		t.Fatalf("old commit: %v", err)
	}
	select {
	case o := <-done:
		if o.err != nil {
			t.Fatalf("release: %v", o.err)
		}
		dedup := 0
		for _, r := range o.res.Rows {
			if r.Deduplicated {
				dedup++
			}
		}
		if len(o.res.Rows) != 3 || dedup != 2 {
			t.Errorf("release rows = %+v, want 3 rows, the 2 write rows deduplicated", o.res.Rows)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("release never finished")
	}
	f.expectRows("after the release", "events", "cut-1", "", legacyRows)
}
