package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// A subscription's period ends at a boundary B, and the billing run closes it
// on a later tick: up to an hour later today, longer after downtime or a
// failed attempt. In that gap the subscription row still describes the ended,
// unbilled period. A write the operator makes in the gap must act on the NEW
// period, as if the billing run had gone first: it closes the ended period
// first, the way an immediate cancel does (Service.Cancel), or it is refused
// with 409 unbilled_period when that close cannot bill.
//
// The rig drives the real server over HTTP with every collaborator wired as
// in production. Time is the wall clock, not a test clock: each request is
// served at a chosen instant (clock.WithEffectiveNow on the request ctx, and
// the server's clock set to the same instant), which is how a request at that
// wall-clock instant is served. The billing run is POST /v1/billing/run, the
// scheduler's run on demand.
//
// Fixture: a 1c/call sum meter; plan A monthly, base 3000c; the subscription
// starts 2026-04-01 (UTC), so P0 = [Apr 1, May 1) and B = May 1.

var (
	lagP0 = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	lagB  = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	lagP2 = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
)

type lagRig struct {
	t        *testing.T
	db       *postgres.DB
	clk      *clock.Fake
	srv      *Server
	auth     string
	timing   string
	meterID  string
	planA    string
	planB    string // monthly, base 6000c, same meter, same timing
	planC    string // monthly, base 1000c, no meter, same timing (an add-on)
	planY    string // yearly, base 30000c, same meter, same timing
	custExt  string
	custID   string
	subID    string
	itemA    string
	eventSeq int
}

func newLagRig(t *testing.T, timing string, withAddOn bool) *lagRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	clk := clock.NewFake(lagP0)
	r := &lagRig{t: t, db: db, clk: clk, srv: NewServer(db, clk, nil), timing: timing, custExt: "lag_cust"}
	tenantID := testutil.CreateTestTenant(t, db, "close lag "+timing)
	r.auth = "Bearer " + createTestAPIKey(t, db, tenantID)

	rule := r.must(http.MethodPost, "/v1/rating-rules", lagP0, map[string]any{
		"rule_key": "calls", "name": "Calls", "mode": "flat", "currency": "USD", "flat_amount_cents": 1,
	}, http.StatusCreated)
	meter := r.must(http.MethodPost, "/v1/meters", lagP0, map[string]any{
		"key": "calls", "name": "Calls", "unit": "calls", "aggregation": "sum", "rating_rule_version_id": rule["id"],
	}, http.StatusCreated)
	r.meterID = meter["id"].(string)
	plan := func(code, interval string, base int64, meters []string) string {
		p := r.must(http.MethodPost, "/v1/plans", lagP0, map[string]any{
			"code": code, "name": code, "currency": "USD", "billing_interval": interval,
			"base_amount_cents": base, "base_bill_timing": timing, "meter_ids": meters,
		}, http.StatusCreated)
		return p["id"].(string)
	}
	r.planA = plan("plan_a", "monthly", 3000, []string{r.meterID})
	r.planB = plan("plan_b", "monthly", 6000, []string{r.meterID})
	r.planC = plan("plan_c", "monthly", 1000, []string{})
	r.planY = plan("plan_y", "yearly", 30000, []string{r.meterID})
	cust := r.must(http.MethodPost, "/v1/customers", lagP0, map[string]any{
		"external_id": r.custExt, "display_name": "Lag Customer", "email": "lag@example.test",
	}, http.StatusCreated)
	r.custID = cust["id"].(string)
	items := []map[string]any{{"plan_id": r.planA, "quantity": 1}}
	if withAddOn {
		items = append(items, map[string]any{"plan_id": r.planC, "quantity": 1})
	}
	sub := r.must(http.MethodPost, "/v1/subscriptions", lagP0, map[string]any{
		"code": "lag-sub", "display_name": "Lag Sub", "customer_id": r.custID, "items": items, "start_now": true,
	}, http.StatusCreated)
	r.subID = sub["id"].(string)
	s := r.sub()
	if !s.CurrentBillingPeriodStart.Equal(lagP0) || !s.CurrentBillingPeriodEnd.Equal(lagB) {
		t.Fatalf("fixture period = [%v, %v), want [%v, %v)", s.CurrentBillingPeriodStart, s.CurrentBillingPeriodEnd, lagP0, lagB)
	}
	for _, it := range s.Items {
		if it.PlanID == r.planA {
			r.itemA = it.ID
		}
	}
	return r
}

// do serves one request at the instant `at`.
func (r *lagRig) do(method, path string, at time.Time, body any) (int, []byte) {
	r.t.Helper()
	r.clk.Set(at)
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req = req.WithContext(clock.WithEffectiveNow(context.Background(), at))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", r.auth)
	rec := httptest.NewRecorder()
	r.srv.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func (r *lagRig) must(method, path string, at time.Time, body any, want int) map[string]any {
	r.t.Helper()
	code, raw := r.do(method, path, at, body)
	if code != want {
		r.t.Fatalf("%s %s at %v: %d %s, want %d", method, path, at, code, raw, want)
	}
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}

func (r *lagRig) sub() domain.Subscription {
	r.t.Helper()
	code, raw := r.do(http.MethodGet, "/v1/subscriptions/"+r.subID, r.clk.Now(context.Background()), nil)
	if code != http.StatusOK {
		r.t.Fatalf("get subscription: %d %s", code, raw)
	}
	var s domain.Subscription
	if err := json.Unmarshal(raw, &s); err != nil {
		r.t.Fatalf("decode subscription: %v", err)
	}
	return s
}

func (r *lagRig) ingest(ts time.Time, qty int64) {
	r.t.Helper()
	r.eventSeq++
	r.must(http.MethodPost, "/v1/usage-events", ts.Add(time.Minute), map[string]any{
		"external_customer_id": r.custExt, "event_name": "calls", "quantity": qty,
		"timestamp": ts.Format(time.RFC3339), "idempotency_key": fmt.Sprintf("lag-%d", r.eventSeq),
	}, http.StatusCreated)
}

func (r *lagRig) run(at time.Time) int {
	r.t.Helper()
	out := r.must(http.MethodPost, "/v1/billing/run", at, nil, http.StatusOK)
	n, _ := out["invoices_generated"].(float64)
	return int(n)
}

type lagLine struct {
	typ    string
	amount int64
	start  *time.Time
}

type lagInvoice struct {
	id, reason, status string
	start, end         time.Time
	subtotal           int64
	lines              []lagLine
}

func (i lagInvoice) sum(typ string) int64 {
	var n int64
	for _, l := range i.lines {
		if l.typ == typ {
			n += l.amount
		}
	}
	return n
}

// invoices reads the subscription's live invoices with their lines.
func (r *lagRig) invoices() []lagInvoice {
	r.t.Helper()
	ctx := context.Background()
	tx, err := r.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(ctx, `SELECT id, billing_reason, status, billing_period_start, billing_period_end, subtotal_cents
		FROM invoices WHERE subscription_id = $1 AND status <> 'voided' ORDER BY billing_period_start, created_at`, r.subID)
	if err != nil {
		r.t.Fatalf("list invoices: %v", err)
	}
	var out []lagInvoice
	for rows.Next() {
		var i lagInvoice
		if err := rows.Scan(&i.id, &i.reason, &i.status, &i.start, &i.end, &i.subtotal); err != nil {
			r.t.Fatalf("scan invoice: %v", err)
		}
		out = append(out, i)
	}
	_ = rows.Close()
	for k := range out {
		lr, err := tx.QueryContext(ctx, `SELECT line_type, amount_cents, billing_period_start FROM invoice_line_items WHERE invoice_id = $1`, out[k].id)
		if err != nil {
			r.t.Fatalf("list lines: %v", err)
		}
		for lr.Next() {
			var l lagLine
			if err := lr.Scan(&l.typ, &l.amount, &l.start); err != nil {
				r.t.Fatalf("scan line: %v", err)
			}
			out[k].lines = append(out[k].lines, l)
		}
		_ = lr.Close()
	}
	return out
}

func (r *lagRig) logInvoices(invs []lagInvoice) {
	r.t.Helper()
	for _, i := range invs {
		r.t.Logf("invoice reason=%s status=%s [%s, %s) subtotal=%dc base=%dc usage=%dc",
			i.reason, i.status, i.start.Format("Jan 2 15:04"), i.end.Format("Jan 2 15:04"), i.subtotal, i.sum("base_fee"), i.sum("usage"))
	}
}

// cycleInvoice returns the one cycle invoice whose header starts at start.
func (r *lagRig) cycleInvoice(invs []lagInvoice, start time.Time) lagInvoice {
	r.t.Helper()
	var found []lagInvoice
	for _, i := range invs {
		if i.reason == string(domain.BillingReasonSubscriptionCycle) && i.start.Equal(start) {
			found = append(found, i)
		}
	}
	if len(found) != 1 {
		r.logInvoices(invs)
		r.t.Fatalf("%d cycle invoices headed at %v, want 1", len(found), start)
	}
	return found[0]
}

func (r *lagRig) creditBalance() int64 {
	r.t.Helper()
	out := r.must(http.MethodGet, "/v1/credits/balance/"+r.custID, r.clk.Now(context.Background()), nil, http.StatusOK)
	n, _ := out["balance_cents"].(float64)
	return int64(n)
}

// wantAPIError checks an error answer: its status, its error code and a part
// of its message.
func wantAPIError(t *testing.T, code int, raw []byte, wantStatus int, wantCode, wantMsg string) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	if code != wantStatus || body.Error.Code != wantCode || !strings.Contains(body.Error.Message, wantMsg) {
		t.Fatalf("answer = %d %s, want %d with code %s and a message containing %q", code, raw, wantStatus, wantCode, wantMsg)
	}
}

// exec runs one fixture statement outside tenant scope.
func (r *lagRig) exec(query string, args ...any) {
	r.t.Helper()
	ctx := context.Background()
	tx, err := r.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		r.t.Fatalf("fixture %q: %v", query, err)
	}
	if err := tx.Commit(); err != nil {
		r.t.Fatalf("commit fixture: %v", err)
	}
}

func skipShort(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
}

// --- D1: cancel schedule ------------------------------------------------------

// TestCloseLag_ScheduleCancelAtPeriodEnd_InGap: the operator schedules
// cancel-at-period-end two hours after B, before the billing run closed P0.
// "Period end" means the end of the period that is running, May's. The
// subscription must stay active through May, and May's prepaid base must be
// billed when P0 closes.
//
// Mutation seam: drop the pre-close from Service.ScheduleCancel → the close
// cancels the subscription retroactively at B and May is never billed.
func TestCloseLag_ScheduleCancelAtPeriodEnd_InGap(t *testing.T) {
	skipShort(t)
	r := newLagRig(t, "in_advance", false)
	r.ingest(lagP0.AddDate(0, 0, 19), 100)
	r.ingest(lagB.Add(time.Hour), 50)

	r.must(http.MethodPost, "/v1/subscriptions/"+r.subID+"/schedule-cancel", lagB.Add(2*time.Hour), map[string]any{"at_period_end": true}, http.StatusOK)
	r.run(lagB.Add(3 * time.Hour))

	s := r.sub()
	invs := r.invoices()
	r.logInvoices(invs)
	t.Logf("after the run: status=%s canceled_at=%v period=[%v, %v) cancel_at_period_end=%v", s.Status, s.CanceledAt, s.CurrentBillingPeriodStart, s.CurrentBillingPeriodEnd, s.CancelAtPeriodEnd)
	if s.Status != domain.SubscriptionActive || !s.CurrentBillingPeriodStart.Equal(lagB) || !s.CancelAtPeriodEnd {
		t.Errorf("status=%s period start=%v cancel_at_period_end=%v, want active in [May 1, Jun 1) and canceling at Jun 1", s.Status, s.CurrentBillingPeriodStart, s.CancelAtPeriodEnd)
	}
	may := r.cycleInvoice(invs, lagB)
	if may.sum("base_fee") != 3000 || may.sum("usage") != 100 {
		t.Errorf("P0's close (header [May 1, Jun 1)): base=%dc usage=%dc, want May's prepaid 3000c base and April's 100c usage", may.sum("base_fee"), may.sum("usage"))
	}
}

// TestCloseLag_ClearScheduledCancel_InGap: the subscription was scheduled to
// cancel at the end of April. Two hours after B the operator clears the
// schedule. The cancel took effect at B; a clear after B cannot undo it. The
// subscription is canceled at B and May is not billed.
func TestCloseLag_ClearScheduledCancel_InGap(t *testing.T) {
	skipShort(t)
	r := newLagRig(t, "in_advance", false)
	r.must(http.MethodPost, "/v1/subscriptions/"+r.subID+"/schedule-cancel", lagP0.AddDate(0, 0, 9), map[string]any{"at_period_end": true}, http.StatusOK)

	code, raw := r.do(http.MethodDelete, "/v1/subscriptions/"+r.subID+"/scheduled-cancel", lagB.Add(2*time.Hour), nil)
	// The answer must say the clear did not happen: a 200 would show the
	// operator "Scheduled cancellation cleared" for a cancel already in force.
	wantAPIError(t, code, raw, http.StatusConflict, "invalid_state", "already took effect")
	r.run(lagB.Add(3 * time.Hour))

	s := r.sub()
	invs := r.invoices()
	r.logInvoices(invs)
	if s.Status != domain.SubscriptionCanceled || s.CanceledAt == nil || !s.CanceledAt.Equal(lagB) {
		t.Errorf("status=%s canceled_at=%v, want canceled at B (%v): the schedule had already taken effect", s.Status, s.CanceledAt, lagB)
	}
	for _, i := range invs {
		if i.sum("base_fee") > 0 && !i.start.Before(lagB) {
			t.Errorf("May base billed (%dc on %s [%v, %v)) after a cancel that took effect at B", i.sum("base_fee"), i.reason, i.start, i.end)
		}
	}
}

// --- D1: collection pause -----------------------------------------------------

// TestCloseLag_PauseCollection_InGap: collection is paused two hours after B.
// April was not paused at its end, so its invoice is finalized for
// collection, not left as a draft.
func TestCloseLag_PauseCollection_InGap(t *testing.T) {
	skipShort(t)
	r := newLagRig(t, "in_arrears", false)
	r.ingest(lagP0.AddDate(0, 0, 19), 100)
	r.must(http.MethodPut, "/v1/subscriptions/"+r.subID+"/pause-collection", lagB.Add(2*time.Hour), map[string]any{"behavior": "keep_as_draft"}, http.StatusOK)
	r.run(lagB.Add(3 * time.Hour))

	invs := r.invoices()
	r.logInvoices(invs)
	apr := r.cycleInvoice(invs, lagP0)
	if apr.status == string(domain.InvoiceDraft) {
		t.Errorf("April's invoice is %s, want finalized: collection was paused after April ended", apr.status)
	}
}

// TestCloseLag_ResumeCollection_InGap: collection was paused during April and
// is resumed two hours after B. April was paused at its end, so its invoice
// stays a draft and is not collected.
func TestCloseLag_ResumeCollection_InGap(t *testing.T) {
	skipShort(t)
	r := newLagRig(t, "in_arrears", false)
	r.ingest(lagP0.AddDate(0, 0, 19), 100)
	r.must(http.MethodPut, "/v1/subscriptions/"+r.subID+"/pause-collection", lagP0.AddDate(0, 0, 9), map[string]any{"behavior": "keep_as_draft"}, http.StatusOK)
	r.must(http.MethodDelete, "/v1/subscriptions/"+r.subID+"/pause-collection", lagB.Add(2*time.Hour), nil, http.StatusOK)
	r.run(lagB.Add(3 * time.Hour))

	invs := r.invoices()
	r.logInvoices(invs)
	apr := r.cycleInvoice(invs, lagP0)
	if apr.status != string(domain.InvoiceDraft) {
		t.Errorf("April's invoice is %s, want draft: collection was paused when April ended", apr.status)
	}
}

// --- D1: item writes ----------------------------------------------------------

// TestCloseLag_ScheduledPlanChange_InGap: two hours after B the operator
// schedules a change to plan B "at period end". The period running is May,
// so the change takes effect on Jun 1, and May's prepaid base is plan A's.
func TestCloseLag_ScheduledPlanChange_InGap(t *testing.T) {
	skipShort(t)
	r := newLagRig(t, "in_advance", false)
	r.must(http.MethodPatch, "/v1/subscriptions/"+r.subID+"/items/"+r.itemA, lagB.Add(2*time.Hour),
		map[string]any{"new_plan_id": r.planB, "immediate": false}, http.StatusOK)
	r.run(lagB.Add(3 * time.Hour))

	invs := r.invoices()
	r.logInvoices(invs)
	may := r.cycleInvoice(invs, lagB)
	if may.sum("base_fee") != 3000 {
		t.Errorf("May's prepaid base = %dc, want 3000c (plan A; the change was scheduled for the end of May)", may.sum("base_fee"))
	}
	s := r.sub()
	if it := s.Items[0]; it.PlanID != r.planA || it.PendingPlanEffectiveAt == nil || !it.PendingPlanEffectiveAt.Equal(lagP2) {
		t.Errorf("item plan=%s pending effective=%v, want plan A with the change pending at %v", it.PlanID, it.PendingPlanEffectiveAt, lagP2)
	}
}

// TestCloseLag_CancelPendingPlanChange_InGap: a change to plan B was scheduled
// in April for the end of April. Two hours after B the operator cancels the
// pending change. It took effect at B; May is billed on plan B.
func TestCloseLag_CancelPendingPlanChange_InGap(t *testing.T) {
	skipShort(t)
	r := newLagRig(t, "in_advance", false)
	r.must(http.MethodPatch, "/v1/subscriptions/"+r.subID+"/items/"+r.itemA, lagP0.AddDate(0, 0, 9),
		map[string]any{"new_plan_id": r.planB, "immediate": false}, http.StatusOK)
	code, raw := r.do(http.MethodDelete, "/v1/subscriptions/"+r.subID+"/items/"+r.itemA+"/pending-change", lagB.Add(2*time.Hour), nil)
	wantAPIError(t, code, raw, http.StatusConflict, "invalid_state", "already took effect")
	r.run(lagB.Add(3 * time.Hour))

	invs := r.invoices()
	r.logInvoices(invs)
	may := r.cycleInvoice(invs, lagB)
	if may.sum("base_fee") != 6000 {
		t.Errorf("May's prepaid base = %dc, want 6000c (plan B; the change took effect at B)", may.sum("base_fee"))
	}
}

// mayBase sums every base line billed for May (its prepaid base, prorations).
func mayBase(invs []lagInvoice) int64 {
	var n int64
	for _, i := range invs {
		for _, l := range i.lines {
			if l.typ == "base_fee" && l.start != nil && !l.start.Before(lagB) && l.start.Before(lagP2) {
				n += l.amount
			}
		}
	}
	return n
}

// TestCloseLag_ItemWrites_InGap: an in_advance item write 36 hours after B
// (the run is late: downtime, or a failed attempt waiting to retry). P0's
// close bills May's prepaid base for the items as they stood at B; the write
// then prorates from its own instant inside May. Before the fix the write
// saw the ended period (no proration: none of it was left) and the late close
// billed the whole of May at the post-write items.
func TestCloseLag_ItemWrites_InGap(t *testing.T) {
	skipShort(t)
	at := lagB.Add(36 * time.Hour)
	cases := []struct {
		name      string
		withAddOn bool
		write     func(r *lagRig) (string, string, any)
		// wantCycleBase is May's prepaid base on P0's close: the items as
		// they stood at B.
		wantCycleBase int64
		// wantProration is the base the write invoices for the rest of May
		// (30 of May's 31 days, from B+36h); wantCredit the credit a
		// removal grants for it.
		wantProration, wantCredit int64
	}{
		{"quantity 1 to 3", false, func(r *lagRig) (string, string, any) {
			return http.MethodPatch, "/v1/subscriptions/" + r.subID + "/items/" + r.itemA, map[string]any{"quantity": 3}
		}, 3000, 5806, 0}, // 2 more units x 3000c x 30/31
		{"immediate plan change A to B", false, func(r *lagRig) (string, string, any) {
			return http.MethodPatch, "/v1/subscriptions/" + r.subID + "/items/" + r.itemA, map[string]any{"new_plan_id": r.planB, "immediate": true}
		}, 3000, 2903, 0}, // (6000c - 3000c) x 30/31
		{"add item C", false, func(r *lagRig) (string, string, any) {
			return http.MethodPost, "/v1/subscriptions/" + r.subID + "/items", map[string]any{"plan_id": r.planC, "quantity": 1}
		}, 3000, 968, 0}, // 1000c x 30/31
		{"remove item C", true, func(r *lagRig) (string, string, any) {
			s := r.sub()
			var c string
			for _, it := range s.Items {
				if it.PlanID == r.planC {
					c = it.ID
				}
			}
			return http.MethodDelete, "/v1/subscriptions/" + r.subID + "/items/" + c, nil
		}, 4000, 0, 968}, // 1000c x 30/31 back as credit
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newLagRig(t, "in_advance", c.withAddOn)
			// A balance that pays May's invoice when P0 closes: an upgrade
			// against an unpaid invoice for the running period is refused
			// (ADR-050), which is not what these cases test.
			r.must(http.MethodPost, "/v1/credits/grant", at.Add(-time.Hour), map[string]any{
				"customer_id": r.custID, "amount_cents": 20000, "description": "close lag fixture",
			}, http.StatusCreated)
			method, path, body := c.write(r)
			code, raw := r.do(method, path, at, body)
			t.Logf("%s %s at B+36h: %d %s", method, path, code, raw)
			if code >= 300 {
				t.Fatalf("write refused: %d %s", code, raw)
			}
			r.run(at.Add(time.Hour))
			invs := r.invoices()
			r.logInvoices(invs)
			t.Logf("May base billed in total: %dc; credit balance %dc", mayBase(invs), r.creditBalance())
			may := r.cycleInvoice(invs, lagB)
			if may.sum("base_fee") != c.wantCycleBase {
				t.Errorf("May's prepaid base on P0's close = %dc, want %dc (the items as they stood at B; the change prorates from B+36h)", may.sum("base_fee"), c.wantCycleBase)
			}
			// The proration is the rest of May, from the write's instant.
			var prorated int64
			for _, i := range invs {
				if i.reason == string(domain.BillingReasonSubscriptionUpdate) {
					if !i.start.Equal(at) || !i.end.Equal(lagP2) {
						t.Errorf("proration invoice headed [%v, %v), want [%v, %v)", i.start, i.end, at, lagP2)
					}
					prorated += i.sum("base_fee")
				}
			}
			if prorated != c.wantProration {
				t.Errorf("proration invoiced = %dc, want %dc for the rest of May", prorated, c.wantProration)
			}
			// May's cycle invoice is paid from the 20000c grant; a removal
			// credits the rest of May back.
			if bal, want := r.creditBalance(), 20000-c.wantCycleBase+c.wantCredit; bal != want {
				t.Errorf("credit balance = %dc, want %dc (20000c - May's %dc + %dc removal credit)", bal, want, c.wantCycleBase, c.wantCredit)
			}
		})
	}
}

// TestCloseLag_ItemWritesWhileBillingFails_Refused: P0 ended and its close
// fails (the tenant's net terms are negative). An item add, a quantity
// change and an item removal in the gap are each refused with 409
// unbilled_period, and none of them is written: acting on the ended period
// would decide what its close bills.
func TestCloseLag_ItemWritesWhileBillingFails_Refused(t *testing.T) {
	skipShort(t)
	r := newLagRig(t, "in_advance", true)
	// A second add-on with no meter, so the add is valid on its own merits.
	planD := r.must(http.MethodPost, "/v1/plans", lagP0, map[string]any{
		"code": "plan_d", "name": "plan_d", "currency": "USD", "billing_interval": "monthly",
		"base_amount_cents": 500, "base_bill_timing": "in_advance", "meter_ids": []string{},
	}, http.StatusCreated)["id"].(string)
	r.exec(`UPDATE tenant_settings SET net_payment_terms = -1 WHERE tenant_id = (SELECT tenant_id FROM subscriptions WHERE id = $1)`, r.subID)
	var addOn string
	for _, it := range r.sub().Items {
		if it.PlanID == r.planC {
			addOn = it.ID
		}
	}
	at := lagB.Add(time.Hour)
	writes := []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/v1/subscriptions/" + r.subID + "/items", map[string]any{"plan_id": planD, "quantity": 1}},
		{http.MethodPatch, "/v1/subscriptions/" + r.subID + "/items/" + r.itemA, map[string]any{"quantity": 3}},
		{http.MethodDelete, "/v1/subscriptions/" + r.subID + "/items/" + addOn, nil},
	}
	for _, w := range writes {
		code, raw := r.do(w.method, w.path, at, w.body)
		wantAPIError(t, code, raw, http.StatusConflict, "unbilled_period", "not invoiced yet")
	}
	s := r.sub()
	if !s.CurrentBillingPeriodStart.Equal(lagP0) || len(s.Items) != 2 {
		t.Fatalf("after the refusals: period start=%v items=%d, want [Apr 1, May 1) with its 2 items", s.CurrentBillingPeriodStart, len(s.Items))
	}
	for _, it := range s.Items {
		if it.Quantity != 1 {
			t.Errorf("item %s quantity = %d, want 1: the refused write changed it", it.PlanID, it.Quantity)
		}
	}
	if s.BillingFailure == nil {
		t.Errorf("billing failure not saved; want the first refused write's attempt recorded")
	}
	for _, i := range r.invoices() {
		if i.reason != string(domain.BillingReasonSubscriptionCreate) {
			t.Errorf("invoice %s [%v, %v) written by a refused write", i.reason, i.start, i.end)
		}
	}
}

// TestCloseLag_NoItemsFailure_AddItemRecovers: the close of P0 fails because
// the subscription has no items, and the saved failure tells the operator to
// add an item, then retry billing. That way out must work in the gap: the
// add is not refused (there is nothing on the ended period to protect), and
// the retry then bills April.
//
// Mutation seam: drop the no-items exemption from CloseEndedPeriodFirst → the
// add answers 409 unbilled_period and the subscription can never be billed.
func TestCloseLag_NoItemsFailure_AddItemRecovers(t *testing.T) {
	skipShort(t)
	r := newLagRig(t, "in_arrears", false)
	r.ingest(lagP0.AddDate(0, 0, 19), 100)
	r.exec(`UPDATE subscription_items SET deleted_at = $1 WHERE id = $2`, lagP0.AddDate(0, 0, 1), r.itemA)

	if code, raw := r.do(http.MethodPost, "/v1/billing/run", lagB.Add(time.Hour), nil); code != http.StatusPartialContent {
		t.Fatalf("billing run with no items: %d %s, want 206 with the failure", code, raw)
	}
	if f := r.sub().BillingFailure; f == nil || !strings.Contains(f.LastError, "Add an item") {
		t.Fatalf("billing failure = %+v, want the no-items failure", f)
	}

	r.must(http.MethodPost, "/v1/subscriptions/"+r.subID+"/items", lagB.Add(2*time.Hour), map[string]any{"plan_id": r.planA, "quantity": 1}, http.StatusCreated)
	out := r.must(http.MethodPost, "/v1/subscriptions/"+r.subID+"/retry-billing", lagB.Add(3*time.Hour), nil, http.StatusOK)
	if n, _ := out["invoices_generated"].(float64); n != 1 {
		t.Fatalf("retry billing generated %v invoices, want 1: %v", out["invoices_generated"], out)
	}
	s := r.sub()
	if s.BillingFailure != nil || !s.CurrentBillingPeriodStart.Equal(lagB) {
		t.Errorf("after the retry: failure=%+v period start=%v, want no failure and the period [May 1, Jun 1)", s.BillingFailure, s.CurrentBillingPeriodStart)
	}
	apr := r.cycleInvoice(r.invoices(), lagP0)
	if apr.sum("usage") != 100 {
		t.Errorf("April usage billed = %dc, want 100c", apr.sum("usage"))
	}
}

// --- D3: cross-interval plan swap -------------------------------------------

// TestCloseLag_CrossIntervalSwap_InGap: two hours after B the operator swaps
// the monthly plan for the yearly one, immediately. April's 100c of usage
// must be billed: the swap must not re-anchor past an ended, unbilled period.
func TestCloseLag_CrossIntervalSwap_InGap(t *testing.T) {
	skipShort(t)
	for _, timing := range []string{"in_advance", "in_arrears"} {
		t.Run(timing, func(t *testing.T) {
			r := newLagRig(t, timing, false)
			r.ingest(lagP0.AddDate(0, 0, 19), 100)
			code, raw := r.do(http.MethodPatch, "/v1/subscriptions/"+r.subID+"/items/"+r.itemA, lagB.Add(2*time.Hour),
				map[string]any{"new_plan_id": r.planY, "immediate": true})
			t.Logf("PATCH swap to yearly at B+2h: %d %s", code, raw)
			r.run(lagB.Add(3 * time.Hour))
			s := r.sub()
			t.Logf("after the run: period=[%v, %v) next=%v", s.CurrentBillingPeriodStart, s.CurrentBillingPeriodEnd, s.NextBillingAt)
			invs := r.invoices()
			r.logInvoices(invs)
			var aprUsage, aprBase int64
			for _, i := range invs {
				for _, l := range i.lines {
					if l.start != nil && l.start.Equal(lagP0) {
						if l.typ == "usage" {
							aprUsage += l.amount
						}
						if l.typ == "base_fee" {
							aprBase += l.amount
						}
					}
				}
			}
			if aprUsage != 100 {
				t.Errorf("April usage billed = %dc, want 100c", aprUsage)
			}
			if aprBase != 3000 {
				t.Errorf("April base billed = %dc, want 3000c: April ran to its end on plan A", aprBase)
			}
		})
	}
}
