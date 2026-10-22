package subscription

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// capturingAudit records audit Log calls so a test can assert what a handler
// wrote, without a real DB. Satisfies auditRecorder.
//
// It captures the emission's CTX SIM BINDING as well as its metadata: since
// ADR-090 §5 the sim axis is derived by the Logger from the ctx clock binding
// (and written to the sim_effective_at / test_clock_id COLUMNS, mirrored into
// metadata), so a fake logger never sees the mirror — what the handler is
// responsible for, and what these tests must therefore assert, is binding the
// clock onto the ctx it emits with.
type capturingAudit struct {
	entries []capturedAudit
}

type capturedAudit struct {
	action string
	meta   map[string]any
	sim    clock.Sim
}

func (c *capturingAudit) Log(ctx context.Context, _, action, _, _, _ string, metadata map[string]any) error {
	sim, _ := clock.SimOf(ctx)
	c.entries = append(c.entries, capturedAudit{action: action, meta: metadata, sim: sim})
	return nil
}

func (c *capturingAudit) Query(_ context.Context, _ string, _ audit.QueryFilter) ([]domain.AuditEntry, int, error) {
	return nil, 0, nil
}

func (c *capturingAudit) firstOf(action string) (capturedAudit, bool) {
	for _, e := range c.entries {
		if e.action == action {
			return e, true
		}
	}
	return capturedAudit{}, false
}

func (c *capturingAudit) actions() []string {
	out := make([]string, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, e.action)
	}
	return out
}

// A plan change on a CLOCK-PINNED subscription must record the test clock +
// simulated effect-time on the audit row (ADR-030 / ADR-090 §5), so the audit
// UI shows the wall-clock click time as the primary timestamp and "Effect on
// test clock at <sim>" as a subline — and so the row is reachable by
// ?test_clock_id= after teardown deletes the sub.
//
// The handler's contract is to BIND the sub's clock onto the emission ctx; the
// Logger stamps the columns from that binding. Pre-fix the item-update writer
// passed the raw payload instead of the sim-carrying metadata — the lone
// subscription audit path that omitted it — so these rows carried no test-clock
// context.
func TestHandler_UpdateItem_PlanChangeAuditCarriesSimContext(t *testing.T) {
	ctx := context.Background()
	tenantID := "t1"

	store := newMemStore()
	now := time.Now().UTC()
	ps := now.Add(-15 * 24 * time.Hour)
	pe := now.Add(15 * 24 * time.Hour)
	sub, err := store.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-clk", DisplayName: "Clock Sub", CustomerID: "cus_1",
		Status:                    domain.SubscriptionActive,
		BillingTime:               domain.BillingTimeCalendar,
		CurrentBillingPeriodStart: &ps,
		CurrentBillingPeriodEnd:   &pe,
		TestClockID:               "tclk_1",
		Items:                     []domain.SubscriptionItem{{PlanID: "plan_old", Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("seed clock-pinned sub: %v", err)
	}
	subID, itemID := sub.ID, sub.Items[0].ID

	// The clock stands still at a simulated instant that is NOT wall-clock now —
	// the whole point of the second axis. It sits inside the sub's current
	// billing period, because wiring the resolver also binds the proration math
	// to simulated time (that is the ADR-029 contract, and this test asserts the
	// proration outcome below).
	frozen := ps.Add(5 * 24 * time.Hour)

	svc := NewService(store, nil)
	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_old": {ID: "plan_old", Name: "Basic", BaseAmountCents: 1000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_new": {ID: "plan_new", Name: "Pro", BaseAmountCents: 3000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	h := NewHandler(svc)
	// The clock resolver is what turns the sub's pin into (frozen_time, clock id)
	// — production wires the billing engine here (router.go).
	h.SetResolver(&stubClockResolver{
		bySub:   map[string]time.Time{subID: frozen},
		clockID: "tclk_1",
	})
	// Paid current-period prebill so the immediate upgrade proceeds (an upgrade
	// against an UNPAID source is blocked per ADR-050 — not what these audit
	// tests are exercising).
	h.SetProrationDeps(plans, &invoicesMock{sourceInvoice: domain.Invoice{ID: "src_inv", PaymentStatus: domain.PaymentSucceeded}})
	rec := &capturingAudit{}
	h.SetAuditLogger(rec)

	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_new", Immediate: true})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)
	rr := httptest.NewRecorder()
	h.updateItem(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}

	entry, ok := rec.firstOf("subscription.item_updated")
	if !ok {
		t.Fatalf("expected a subscription.item_updated audit row; got actions=%v", rec.actions())
	}
	if entry.meta["action"] != "item_plan_changed" {
		t.Errorf("meta.action: got %v, want item_plan_changed", entry.meta["action"])
	}
	if entry.sim.TestClockID != "tclk_1" {
		t.Errorf("emission ctx test_clock_id: got %q, want tclk_1 — sim context dropped (the bug)", entry.sim.TestClockID)
	}
	if !entry.sim.At.Equal(frozen) {
		t.Errorf("emission ctx sim instant: got %v, want %v (the clock's frozen_time)", entry.sim.At, frozen)
	}
	// This upgrade runs the LEGACY (non-atomic) proration path — h.db is
	// unwired here. The audit row must STILL carry the proration outcome,
	// because the audit write was moved to after proration resolves. Pre-
	// fix the legacy path wrote the row before result.Proration was set, so
	// the timeline showed "Plan changed" with no amount on cross-interval /
	// unwired-db changes.
	if entry.meta["proration_type"] != "invoice" {
		t.Errorf("meta.proration_type: got %v, want invoice (legacy path must stamp the outcome too)", entry.meta["proration_type"])
	}
	if _, ok := entry.meta["proration_amount_cents"]; !ok {
		t.Errorf("meta.proration_amount_cents missing — the legacy-path proration outcome was dropped from the audit row")
	}
}

// A plan change scheduled for renewal names the plan it changes from: its
// line reads "Basic → Pro at renewal on <date>" (S-17), not "at renewal on
// <date>" with no plans. The item keeps its plan until the boundary, and the
// handler reads the old plan before the change only for changes that prorate,
// so the scheduled row was written with an empty old_plan_id.
func TestHandler_UpdateItem_ScheduledPlanChangeAuditNamesBothPlans(t *testing.T) {
	ctx := context.Background()
	tenantID := "t1"

	store := newMemStore()
	subID, itemID := seedSubWithItem(t, store, tenantID, "cus_1", "plan_old")

	svc := NewService(store, nil)
	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_old": {ID: "plan_old", Name: "Basic", BaseAmountCents: 1000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_new": {ID: "plan_new", Name: "Pro", BaseAmountCents: 3000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	h := NewHandler(svc)
	h.SetProrationDeps(plans, &invoicesMock{sourceInvoice: domain.Invoice{ID: "src_inv", PaymentStatus: domain.PaymentSucceeded}})
	rec := &capturingAudit{}
	h.SetAuditLogger(rec)

	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_new", Immediate: false})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)
	rr := httptest.NewRecorder()
	h.updateItem(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}

	entry, ok := rec.firstOf(domain.AuditActionSubscriptionItemUpdated)
	if !ok {
		t.Fatalf("expected a subscription.item_updated audit row; got actions=%v", rec.actions())
	}
	if entry.meta["old_plan_id"] != "plan_old" || entry.meta["new_plan_id"] != "plan_new" {
		t.Errorf("old_plan_id, new_plan_id: got %v, %v; want plan_old, plan_new", entry.meta["old_plan_id"], entry.meta["new_plan_id"])
	}
	effectiveAt, ok := entry.meta["effective_at"].(time.Time)
	if !ok {
		t.Fatalf("effective_at: got %T %v, want the boundary the change lands on", entry.meta["effective_at"], entry.meta["effective_at"])
	}

	l := describeSubscriptionAction(entry.action, entry.meta, lineLabels{plans: map[string]string{"plan_old": "Basic", "plan_new": "Pro"}})
	if l.Title != "Plan change scheduled" {
		t.Errorf("title: got %q, want Plan change scheduled", l.Title)
	}
	if got, ts := legacyDetail(l); got != "Basic → Pro at renewal on" || ts != effectiveAt.UTC().Format(time.RFC3339) {
		t.Errorf("detail: got %q, %q; want %q, %q", got, ts, "Basic → Pro at renewal on", effectiveAt.UTC().Format(time.RFC3339))
	}
}

// A plan change on a WALL-CLOCK (non-pinned) subscription must NOT inject
// sim-time context — h.auditCtx is a no-op there, so the row's sim columns stay
// NULL and it never appears in the simulated slice.
func TestHandler_UpdateItem_PlanChangeAuditNoSimContextWhenUnpinned(t *testing.T) {
	ctx := context.Background()
	tenantID := "t1"

	store := newMemStore()
	subID, itemID := seedSubWithItem(t, store, tenantID, "cus_1", "plan_old")

	svc := NewService(store, nil)
	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_old": {ID: "plan_old", Name: "Basic", BaseAmountCents: 1000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_new": {ID: "plan_new", Name: "Pro", BaseAmountCents: 3000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	h := NewHandler(svc)
	// Paid current-period prebill so the immediate upgrade proceeds (an upgrade
	// against an UNPAID source is blocked per ADR-050 — not what these audit
	// tests are exercising).
	h.SetProrationDeps(plans, &invoicesMock{sourceInvoice: domain.Invoice{ID: "src_inv", PaymentStatus: domain.PaymentSucceeded}})
	rec := &capturingAudit{}
	h.SetAuditLogger(rec)

	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_new", Immediate: true})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)
	rr := httptest.NewRecorder()
	h.updateItem(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}

	entry, ok := rec.firstOf("subscription.item_updated")
	if !ok {
		t.Fatalf("expected a subscription.item_updated audit row; got actions=%v", rec.actions())
	}
	if entry.sim.Simulated() {
		t.Errorf("emission ctx carries a clock on a wall-clock sub: %+v", entry.sim)
	}
}
