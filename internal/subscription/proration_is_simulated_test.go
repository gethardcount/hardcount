package subscription

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// seedSubWithClockAt is seedSubWithItemAt plus an explicit test_clock_id, so a
// test can exercise both a clock-pinned and a wall-clock subscription.
func seedSubWithClockAt(t *testing.T, store *memStore, tenantID, custID, plan string, ps, pe time.Time, clockID string) (string, string) {
	t.Helper()
	sub, err := store.Create(context.Background(), tenantID, domain.Subscription{
		Code:        fmt.Sprintf("sub-%d", len(store.subs)+1),
		DisplayName: "Test Sub", CustomerID: custID,
		Status:                    domain.SubscriptionActive,
		BillingTime:               domain.BillingTimeCalendar,
		CurrentBillingPeriodStart: &ps,
		CurrentBillingPeriodEnd:   &pe,
		TestClockID:               clockID,
		Items:                     []domain.SubscriptionItem{{PlanID: plan, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
	return sub.ID, sub.Items[0].ID
}

// TestUpdateItem_PlanChange_StampsIsSimulatedFromTestClock locks the fix for the
// missing simulation marker on a dashboard plan-change proration invoice. The
// invoice handleItemProration builds must carry is_simulated = (sub on a test
// clock), matching every engine invoice path (billOnePeriod / BillOnCreate /
// threshold). Pre-fix it defaulted to Go's zero value false, so a clock-pinned
// upgrade's invoice — whose dates are on simulation time — persisted
// is_simulated=false and the dashboard showed no "Simulated" marker while the
// sibling cycle invoice (engine path) did. The frontend reads this field
// authoritatively and deliberately does not infer simulation from a future
// date, so the backend must stamp it correctly here.
func TestUpdateItem_PlanChange_StampsIsSimulatedFromTestClock(t *testing.T) {
	cases := []struct {
		name        string
		testClockID string
		want        bool
	}{
		{"clock-pinned sub → simulated invoice", "tc_1", true},
		{"wall-clock sub → real invoice", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := clock.WithEffectiveNow(context.Background(), proNow) // 15 of 30 days remain
			tenantID := "t1"

			store := newMemStore()
			subID, itemID := seedSubWithClockAt(t, store, tenantID, "cus_1", "plan_starter", proPeriodStart, proPeriodEnd, tc.testClockID)
			svc := NewService(store, nil)

			plans := &plansMock{plans: map[string]domain.Plan{
				"plan_starter": {ID: "plan_starter", Name: "Starter", BaseAmountCents: 2000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
				"plan_pro":     {ID: "plan_pro", Name: "Pro", BaseAmountCents: 5000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
			}}
			invoices := &invoicesMock{sourceInvoice: domain.Invoice{ID: "src_inv", PaymentStatus: domain.PaymentSucceeded}}

			h := NewHandler(svc)
			h.SetProrationDeps(plans, invoices)

			body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_pro", Immediate: true})
			req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)
			rr := httptest.NewRecorder()
			h.updateItem(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
			}
			if len(invoices.createdInvoices) != 1 {
				t.Fatalf("createdInvoices: got %d, want 1", len(invoices.createdInvoices))
			}
			if got := invoices.createdInvoices[0].IsSimulated; got != tc.want {
				t.Errorf("proration invoice IsSimulated = %v, want %v (test_clock_id=%q)", got, tc.want, tc.testClockID)
			}
		})
	}
}
