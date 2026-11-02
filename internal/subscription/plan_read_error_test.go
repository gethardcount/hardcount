package subscription

import (
	"context"
	"errors"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
)

// planReaderBlip answers every GetPlan with a non-not-found failure — the
// shape of a DB blip while reading a plan that exists (#1560).
type planReaderBlip struct{ err error }

func (p planReaderBlip) GetPlan(context.Context, string, string) (domain.Plan, error) {
	return domain.Plan{}, p.err
}

// TestCreate_PlanReadErrorIsServerFault: a plan-reader failure that is not
// ErrNotFound must not become 422 "plan not found". Create reaches
// rejectArchivedPlans / rejectMeterOverlap / rejectCurrencyMismatch.
func TestCreate_PlanReadErrorIsServerFault(t *testing.T) {
	blip := errors.New("connection reset by peer")
	svc := NewService(newMemStore(), nil)
	svc.SetCustomerReader(&fakeCustomerReader{cust: domain.Customer{ID: "cus_1", Status: domain.CustomerStatusActive}})
	svc.SetPlanReader(planReaderBlip{err: blip})

	_, err := svc.Create(context.Background(), "t1", CreateInput{
		Code: "sub-blip", DisplayName: "S", CustomerID: "cus_1",
		Items: []CreateItemInput{{PlanID: "pln_1", Quantity: 1}},
	})
	if err == nil {
		t.Fatal("create with plan-read blip: err = nil")
	}
	if errors.Is(err, errs.ErrValidation) {
		t.Fatalf("create with plan-read blip: got validation/422 %v; want wrapped server error", err)
	}
	if !errors.Is(err, blip) {
		t.Fatalf("create with plan-read blip: err = %v, want wrapped %v", err, blip)
	}
}

// TestCreate_MissingPlanIsValidation: genuine ErrNotFound still answers 422.
func TestCreate_MissingPlanIsValidation(t *testing.T) {
	svc := NewService(newMemStore(), nil)
	svc.SetCustomerReader(&fakeCustomerReader{cust: domain.Customer{ID: "cus_1", Status: domain.CustomerStatusActive}})
	svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{}})

	_, err := svc.Create(context.Background(), "t1", CreateInput{
		Code: "sub-missing", DisplayName: "S", CustomerID: "cus_1",
		Items: []CreateItemInput{{PlanID: "pln_gone", Quantity: 1}},
	})
	if !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("missing plan: err = %v, want validation/422", err)
	}
}

// TestRejectMixedItemIntervals_PlanReadErrorIsServerFault covers the
// multi-item Create / swap interval guard (#1560 sites :420, :433).
func TestRejectMixedItemIntervals_PlanReadErrorIsServerFault(t *testing.T) {
	blip := errors.New("plan store timeout")
	svc := NewService(newMemStore(), nil)
	svc.SetPlanReader(planReaderBlip{err: blip})

	err := svc.rejectMixedItemIntervals(context.Background(), "t1", []domain.SubscriptionItem{
		{PlanID: "pln_a"}, {PlanID: "pln_b"},
	})
	if errors.Is(err, errs.ErrValidation) || !errors.Is(err, blip) {
		t.Fatalf("mixed-interval plan blip: err = %v, want wrapped blip not 422", err)
	}
}

// TestUpdateItem_CurrentPlanReadErrorIsServerFault: the plan already on the
// item cannot be deleted, so a missing/blipped read is a 500, not 422.
func TestUpdateItem_CurrentPlanReadErrorIsServerFault(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	svc := NewService(store, nil)
	svc.SetCustomerReader(&fakeCustomerReader{cust: domain.Customer{ID: "cus_1", Status: domain.CustomerStatusActive}})
	svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
		"pln_old": {ID: "pln_old", Currency: "USD", Status: domain.PlanActive, BillingInterval: domain.BillingMonthly},
		"pln_new": {ID: "pln_new", Currency: "USD", Status: domain.PlanActive, BillingInterval: domain.BillingMonthly},
	}})
	sub, err := svc.Create(ctx, "t1", CreateInput{
		Code: "sub-swap", DisplayName: "S", CustomerID: "cus_1",
		Items: []CreateItemInput{{PlanID: "pln_old", Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("seed create: %v", err)
	}
	itemID := sub.Items[0].ID

	blip := errors.New("connection reset by peer")
	svc.SetPlanReader(planReaderBlip{err: blip})
	_, err = svc.UpdateItem(ctx, "t1", sub.ID, itemID, UpdateItemInput{NewPlanID: "pln_new"})
	if err == nil {
		t.Fatal("update-item with plan-read blip: err = nil")
	}
	if errors.Is(err, errs.ErrValidation) {
		t.Fatalf("update-item plan blip: got 422 %v; want server fault", err)
	}
	if !errors.Is(err, blip) {
		t.Fatalf("update-item plan blip: err = %v, want wrapped %v", err, blip)
	}
}
