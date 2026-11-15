package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/billing"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/testclock"
)

// goneClocks answers every test clock as not found: a clock deleted in a
// race with the read.
type goneClocks struct{ billing.TestClockReader }

func (goneClocks) Get(_ context.Context, _, id string) (domain.TestClock, error) {
	return domain.TestClock{}, fmt.Errorf("test clock %s: %w", id, errs.ErrNotFound)
}

// On the real chain: a create-preview for a clock customer's subscription is
// generated at the clock's time, refused for retry (503) while the clock
// cannot be read, and at real time when the clock is not found.
func TestE2E_CreatePreviewIsAtTheSubscriptionsClockTime(t *testing.T) {
	r := newDatesRig(t, "Preview Time Co")
	frozen := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	clockID := r.newClock("preview time", frozen)
	cust := flowStr(r.call("POST", "/v1/customers", flowReq{body: map[string]any{
		"external_id": "preview-time", "display_name": "Preview time", "test_clock_id": clockID,
	}}).want(t, 201, "create clock customer").body, "id")
	r.call("POST", "/v1/subscriptions", flowReq{body: map[string]any{
		"code": "preview-time", "display_name": "Preview time", "customer_id": cust,
		"items": []map[string]any{{"plan_id": r.planID, "quantity": 1}}, "billing_time": "anniversary", "trial_days": 14,
	}}).want(t, 201, "create subscription")
	preview := flowReq{body: map[string]any{"customer_id": cust}}

	got := r.call("POST", "/v1/invoices/create_preview", preview).want(t, 200, "preview").body
	if at, err := time.Parse(time.RFC3339Nano, flowStr(got, "generated_at")); err != nil || !at.Equal(frozen) {
		t.Errorf("generated_at %q (%v); want the clock's %s", flowStr(got, "generated_at"), err, frozen)
	}

	clocks := &unreadableClocks{TestClockReader: testclock.NewPostgresStore(r.db)}
	r.srv.BillingEngine.SetTestClockReader(clocks)
	clocks.fail.Store(true)
	if got := r.call("POST", "/v1/invoices/create_preview", preview); got.status != 503 {
		t.Errorf("preview while the clock cannot be read: %d %s; want 503", got.status, got.raw)
	}

	r.srv.BillingEngine.SetTestClockReader(goneClocks{})
	from := time.Now().UTC()
	gone := r.call("POST", "/v1/invoices/create_preview", preview).want(t, 200, "preview when the clock is not found").body
	if at, err := time.Parse(time.RFC3339Nano, flowStr(gone, "generated_at")); err != nil || at.Before(from.Add(-time.Second)) || at.After(time.Now().UTC().Add(time.Second)) {
		t.Errorf("generated_at %q (%v); want real time", flowStr(gone, "generated_at"), err)
	}
}
