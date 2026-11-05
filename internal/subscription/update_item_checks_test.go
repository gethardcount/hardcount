package subscription

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// UpdateItem and UpdateItemTx run one set of checks (checkItemChange), so a
// request gets the same answer whichever path the handler sends it down. The
// immediate cross-interval swap is the one difference: UpdateItemTx writes it
// and UpdateItem refuses it, which other tests pin.
func TestUpdateItem_BothEntryPointsCheckAlike(t *testing.T) {
	now := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
	ctx := context.Background()
	plans := &fakePlanReader{plans: map[string]domain.Plan{
		"p_a":    {ID: "p_a", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 1000},
		"p_b":    {ID: "p_b", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 2000},
		"p_c":    {ID: "p_c", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 3000},
		"p_arr":  {ID: "p_arr", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears, BaseAmountCents: 1000},
		"p_year": {ID: "p_year", BillingInterval: domain.BillingYearly, BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 10000},
		"p_arch": {ID: "p_arch", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 1000, Status: domain.PlanArchived},
	}}
	// newSub is an active two-item monthly in_advance subscription on its
	// own store, so one path's write cannot change what the other checks.
	newSub := func(t *testing.T) (*Service, domain.Subscription) {
		t.Helper()
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(plans)
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items:       []CreateItemInput{{PlanID: "p_a"}, {PlanID: "p_b"}},
			BillingTime: domain.BillingTimeAnniversary,
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		return svc, sub
	}
	qty := func(n int64) *int64 { return &n }

	cases := []struct {
		name  string
		item  string // "" = the subscription's first item
		input UpdateItemInput
	}{
		{"neither field", "", UpdateItemInput{}},
		{"both fields", "", UpdateItemInput{Quantity: qty(2), NewPlanID: "p_c"}},
		{"quantity below one", "", UpdateItemInput{Quantity: qty(0)}},
		{"unknown item", "hc_si_missing", UpdateItemInput{Quantity: qty(2)}},
		{"same plan", "", UpdateItemInput{NewPlanID: "p_a", Immediate: true}},
		{"unknown plan, immediate", "", UpdateItemInput{NewPlanID: "p_missing", Immediate: true}},
		{"unknown plan, scheduled", "", UpdateItemInput{NewPlanID: "p_missing"}},
		{"archived plan, immediate", "", UpdateItemInput{NewPlanID: "p_arch", Immediate: true}},
		{"archived plan, scheduled", "", UpdateItemInput{NewPlanID: "p_arch"}},
		{"other bill timing, immediate", "", UpdateItemInput{NewPlanID: "p_arr", Immediate: true}},
		{"other bill timing, scheduled", "", UpdateItemInput{NewPlanID: "p_arr"}},
		{"mixed intervals, immediate", "", UpdateItemInput{NewPlanID: "p_year", Immediate: true}},
		{"mixed intervals, scheduled", "", UpdateItemInput{NewPlanID: "p_year"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svcOwn, subOwn := newSub(t)
			svcTx, subTx := newSub(t)
			itemOwn, itemTx := subOwn.Items[0].ID, subTx.Items[0].ID
			if tc.item != "" {
				itemOwn, itemTx = tc.item, tc.item
			}
			_, errOwn := svcOwn.UpdateItem(ctx, "t1", subOwn.ID, itemOwn, tc.input)
			_, errTx := svcTx.UpdateItemTx(ctx, nil, "t1", subTx.ID, itemTx, tc.input)
			if errOwn == nil || errTx == nil {
				t.Fatalf("want both refused; UpdateItem: %v, UpdateItemTx: %v", errOwn, errTx)
			}
			// Subscription IDs come from per-store counters, so both stores
			// name the subscription alike and the messages compare as is.
			if errOwn.Error() != errTx.Error() || errs.Field(errOwn) != errs.Field(errTx) || errs.Code(errOwn) != errs.Code(errTx) {
				t.Errorf("UpdateItem: %v (field %q, code %q); UpdateItemTx: %v (field %q, code %q)",
					errOwn, errs.Field(errOwn), errs.Code(errOwn), errTx, errs.Field(errTx), errs.Code(errTx))
			}
		})
	}

	t.Run("a scheduled change through UpdateItemTx records the pending plan", func(t *testing.T) {
		svc, sub := newSub(t)
		item := sub.Items[0]
		res, err := svc.UpdateItemTx(ctx, nil, "t1", sub.ID, item.ID, UpdateItemInput{NewPlanID: "p_c"})
		if err != nil {
			t.Fatalf("scheduled change: %v", err)
		}
		if res.Item.PlanID != "p_a" || res.Item.PendingPlanID != "p_c" {
			t.Errorf("item plan %q pending %q, want p_a pending p_c", res.Item.PlanID, res.Item.PendingPlanID)
		}
		if res.Item.PendingPlanEffectiveAt == nil || !res.Item.PendingPlanEffectiveAt.Equal(*sub.CurrentBillingPeriodEnd) {
			t.Errorf("pending plan effective at %v, want the period end %v", res.Item.PendingPlanEffectiveAt, sub.CurrentBillingPeriodEnd)
		}
	})
}

// The API refuses an unknown new_plan_id with a 422 whose param is
// new_plan_id. The request here is a scheduled change on a two-item
// subscription, which the handler sends to UpdateItem. The mixed-interval
// check reads the new plan too and would name param items, so the plan
// read has to run before it.
func TestUpdateItem_UnknownNewPlanAnswersOnNewPlanID(t *testing.T) {
	now := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
	ctx := auth.WithTenantID(context.Background(), "t1")
	plans := &fakePlanReader{plans: map[string]domain.Plan{
		"p_a": {ID: "p_a", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 1000, Currency: "USD"},
		"p_b": {ID: "p_b", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 2000, Currency: "USD"},
	}}
	store := newMemStore()
	svc := NewService(store, clock.NewFake(now))
	svc.SetPlanReader(plans)
	sub, err := svc.Create(ctx, "t1", CreateInput{
		Code: "s", DisplayName: "n", CustomerID: "c",
		Items:       []CreateItemInput{{PlanID: "p_a"}, {PlanID: "p_b"}},
		BillingTime: domain.BillingTimeAnniversary,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Wired as production wires it, so the handler routes the request as it
	// would there.
	h := NewHandler(svc)
	h.SetProrationDeps(plans, &invoicesMock{})

	itemID := sub.Items[0].ID
	rr := httptest.NewRecorder()
	h.updateItem(rr, updateItemURL(ctx, sub.ID, itemID, []byte(`{"new_plan_id":"p_missing"}`)))

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422; body %s", rr.Code, rr.Body.String())
	}
	var body respond.ErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the error body: %v; body %s", err, rr.Body.String())
	}
	if got := body.Error; got.Param != "new_plan_id" || got.Code != "validation_error" || got.Message != `plan "p_missing" not found` {
		t.Errorf("error param %q code %q message %q, want param new_plan_id, code validation_error, message %q",
			got.Param, got.Code, got.Message, `plan "p_missing" not found`)
	}
	item, err := store.GetItem(ctx, "t1", itemID)
	if err != nil {
		t.Fatalf("reload the item: %v", err)
	}
	if item.PlanID != "p_a" || item.PendingPlanID != "" {
		t.Errorf("item plan %q pending %q, want p_a and no pending plan", item.PlanID, item.PendingPlanID)
	}
}
