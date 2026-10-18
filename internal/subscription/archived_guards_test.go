package subscription

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

type fakeCustomerReader struct{ cust domain.Customer }

func (f *fakeCustomerReader) Get(_ context.Context, _, _ string) (domain.Customer, error) {
	return f.cust, nil
}

// TestCreate_RejectsArchivedCustomer locks ADR-067: subscription.Create for
// an archived customer is a 409 — archive means "no new business", and
// without this a direct API call starts billing a retired customer.
// Mutation seam: drop the status check in Create's customer fetch.
func TestCreate_RejectsArchivedCustomer(t *testing.T) {
	svc := NewService(newMemStore(), nil)
	svc.SetCustomerReader(&fakeCustomerReader{cust: domain.Customer{
		ID: "cus_1", Status: domain.CustomerStatusArchived,
	}})
	_, err := svc.Create(context.Background(), "t1", CreateInput{
		Code: "sub-arch", DisplayName: "S", CustomerID: "cus_1",
		Items: []CreateItemInput{{PlanID: "pln_1", Quantity: 1}},
	})
	if !errors.Is(err, errs.ErrInvalidState) {
		t.Fatalf("create for archived customer: err = %v, want InvalidState (409)", err)
	}
}

// TestCreate_RejectsArchivedPlan: an archived plan accepts no new
// subscriptions (it previously remained fully subscribable via the API).
func TestCreate_RejectsArchivedPlan(t *testing.T) {
	svc := NewService(newMemStore(), nil)
	svc.SetCustomerReader(&fakeCustomerReader{cust: domain.Customer{ID: "cus_1", Status: domain.CustomerStatusActive}})
	svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
		"pln_arch": {ID: "pln_arch", Currency: "USD", Status: domain.PlanArchived},
	}})
	_, err := svc.Create(context.Background(), "t1", CreateInput{
		Code: "sub-planarch", DisplayName: "S", CustomerID: "cus_1",
		Items: []CreateItemInput{{PlanID: "pln_arch", Quantity: 1}},
	})
	if !errors.Is(err, errs.ErrInvalidState) {
		t.Fatalf("create on archived plan: err = %v, want InvalidState (409)", err)
	}

	// Control: active plan passes the guard (may fail later for other
	// reasons, but NOT with the archived-plan 409).
	svc2 := NewService(newMemStore(), nil)
	svc2.SetCustomerReader(&fakeCustomerReader{cust: domain.Customer{ID: "cus_1", Status: domain.CustomerStatusActive}})
	svc2.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
		"pln_ok": {ID: "pln_ok", Currency: "USD", Status: domain.PlanActive, BillingInterval: domain.BillingMonthly},
	}})
	if _, err := svc2.Create(context.Background(), "t1", CreateInput{
		Code: "sub-ok", DisplayName: "S", CustomerID: "cus_1",
		Items: []CreateItemInput{{PlanID: "pln_ok", Quantity: 1}},
	}); errors.Is(err, errs.ErrInvalidState) {
		t.Fatalf("active plan tripped the archived guard: %v", err)
	}
}

// TestAddItem_RejectsArchivedPlan (#1194): adding an item puts new business
// on a plan, so an archived plan is refused on both add paths (AddItem and
// the handler's in-transaction AddItemTx) with Create's 409, and nothing is
// added. The real-Postgres twin through the HTTP handler is
// TestAddItem_ArchivedPlan_RefusedOnBothPaths.
// Mutation seam: drop the archived check from the shared add-item validation.
func TestAddItem_RejectsArchivedPlan(t *testing.T) {
	ctx := context.Background()
	paths := map[string]func(svc *Service, subID, planID string) (domain.SubscriptionItem, error){
		"AddItem": func(svc *Service, subID, planID string) (domain.SubscriptionItem, error) {
			return svc.AddItem(ctx, "t1", subID, AddItemInput{PlanID: planID, Quantity: 1})
		},
		"AddItemTx": func(svc *Service, subID, planID string) (domain.SubscriptionItem, error) {
			return svc.AddItemTx(ctx, nil, "t1", subID, AddItemInput{PlanID: planID, Quantity: 1})
		},
	}
	for name, add := range paths {
		t.Run(name, func(t *testing.T) {
			store := newMemStore()
			svc := NewService(store, nil)
			svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
				"pln_base": {ID: "pln_base", Currency: "USD", Status: domain.PlanActive, BillingInterval: domain.BillingMonthly},
				"pln_arch": {ID: "pln_arch", Currency: "USD", Status: domain.PlanArchived, BillingInterval: domain.BillingMonthly},
				"pln_ok":   {ID: "pln_ok", Currency: "USD", Status: domain.PlanActive, BillingInterval: domain.BillingMonthly},
			}})
			sub, err := store.Create(ctx, "t1", domain.Subscription{
				Code: "sub-add-arch", DisplayName: "S", CustomerID: "cus_1",
				Status: domain.SubscriptionActive,
				Items:  []domain.SubscriptionItem{{PlanID: "pln_base", Quantity: 1}},
			})
			if err != nil {
				t.Fatalf("seed sub: %v", err)
			}

			_, err = add(svc, sub.ID, "pln_arch")
			if !errors.Is(err, errs.ErrInvalidState) || !strings.Contains(err.Error(), `plan "pln_arch" is archived`) {
				t.Fatalf("add item on archived plan: err = %v, want InvalidState naming the archived plan (409)", err)
			}
			after, err := svc.Get(ctx, "t1", sub.ID)
			if err != nil {
				t.Fatalf("get sub: %v", err)
			}
			if len(after.Items) != 1 {
				t.Fatalf("refused add must add nothing: got %d items, want 1", len(after.Items))
			}

			// Control: an active plan passes on the same path.
			if _, err := add(svc, sub.ID, "pln_ok"); err != nil {
				t.Fatalf("add item on active plan: %v", err)
			}
		})
	}
}
