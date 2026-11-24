package subscription

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// capturingAudit records audit Log calls so a test can assert what a handler
// wrote, without a real DB. Satisfies auditRecorder. It captures the instant
// the emission's ctx carries as well as its metadata.
type capturingAudit struct {
	entries []capturedAudit
}

type capturedAudit struct {
	action string
	meta   map[string]any
	at     time.Time
}

func (c *capturingAudit) Log(ctx context.Context, _, action, _, _, _ string, metadata map[string]any) error {
	at, _ := clock.EffectiveNow(ctx)
	c.entries = append(c.entries, capturedAudit{action: action, meta: metadata, at: at})
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

// A plan change's audit row runs on the request's instant, which the API's
// door binds, and carries the proration outcome.
func TestHandler_UpdateItem_PlanChangeAuditRunsOnTheRequestsInstant(t *testing.T) {
	ctx := postgres.WithLivemode(context.Background(), true)
	tenantID := "t1"

	store := newMemStore()
	now := time.Now().UTC()
	ps := now.Add(-15 * 24 * time.Hour)
	pe := now.Add(15 * 24 * time.Hour)
	sub, err := store.CreateWithBill(ctx, tenantID, domain.Subscription{
		Code: "sub-time", DisplayName: "Time Sub", CustomerID: "cus_1",
		Status:                    domain.SubscriptionActive,
		BillingTime:               domain.BillingTimeCalendar,
		CurrentBillingPeriodStart: &ps,
		CurrentBillingPeriodEnd:   &pe,
		Items:                     []domain.SubscriptionItem{{PlanID: "plan_old", Quantity: 1}},
	}, nil)
	if err != nil {
		t.Fatalf("seed sub: %v", err)
	}
	subID, itemID := sub.ID, sub.Items[0].ID

	// The request's instant is not wall-clock now, so a row stamped from the
	// machine's clock shows. It sits inside the sub's current billing period,
	// because the proration math runs on it too (this test asserts the
	// proration outcome below).
	frozen := ps.Add(5 * 24 * time.Hour)

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
	// The API's door binds the request's instant before the handler runs.
	decided := clock.WithEffectiveNow(auth.WithTenantID(ctx, tenantID), frozen)
	req := updateItemURL(decided, subID, itemID, body)
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
	if !entry.at.Equal(frozen) {
		t.Errorf("emission ctx instant: got %v, want the request's %v", entry.at, frozen)
	}
	// The audit row carries the proration outcome: the handler writes it
	// after the proration resolves, so the timeline shows the amount the
	// change billed, not just "Plan changed".
	if entry.meta["proration_type"] != "invoice" {
		t.Errorf("meta.proration_type: got %v, want invoice (the row must stamp the outcome)", entry.meta["proration_type"])
	}
	if _, ok := entry.meta["proration_amount_cents"]; !ok {
		t.Errorf("meta.proration_amount_cents missing — the proration outcome was dropped from the audit row")
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
	req := updateItemURL(auth.WithTenantID(ctx, tenantID), subID, itemID, body)
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
