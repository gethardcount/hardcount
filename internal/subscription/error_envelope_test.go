package subscription

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

type errorEnvelope struct {
	Error struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
		Param   string `json:"param"`
	} `json:"error"`
}

func decodeEnvelope(t *testing.T, rr *httptest.ResponseRecorder) errorEnvelope {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v; body=%s", err, rr.Body.String())
	}
	return env
}

// The multi-currency refusal puts each part of the error in its own envelope
// field, so a client switching on type or code recognises it.
func TestSetBillingThresholds_MultiCurrencyErrorEnvelope(t *testing.T) {
	store := newMemStore()
	now := time.Now().UTC()
	start, end := now.Add(-15*24*time.Hour), now.Add(15*24*time.Hour)
	sub, err := store.CreateWithBill(context.Background(), "t1", domain.Subscription{
		Code: "sub-mc", CustomerID: "cus_1",
		Status:                    domain.SubscriptionActive,
		BillingTime:               domain.BillingTimeCalendar,
		CurrentBillingPeriodStart: &start,
		CurrentBillingPeriodEnd:   &end,
		Items: []domain.SubscriptionItem{
			{PlanID: "plan_usd", Quantity: 1},
			{PlanID: "plan_eur", Quantity: 1},
		},
	}, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	h := NewHandler(NewService(store, nil))
	h.SetProrationDeps(&plansMock{plans: map[string]domain.Plan{
		"plan_usd": {ID: "plan_usd", Currency: "USD"},
		"plan_eur": {ID: "plan_eur", Currency: "EUR"},
	}}, &invoicesMock{})

	body, _ := json.Marshal(BillingThresholdsInput{AmountGTE: 5000})
	req := withTenantRoute(httptest.NewRequest(http.MethodPut, "/subscriptions/"+sub.ID+"/billing-thresholds", bytes.NewReader(body)), "t1", map[string]string{"id": sub.ID})
	rr := httptest.NewRecorder()
	h.setBillingThresholds(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
	env := decodeEnvelope(t, rr)
	if env.Error.Type != "invalid_request_error" || env.Error.Code != "validation_error" ||
		env.Error.Param != "billing_thresholds" ||
		env.Error.Message != "billing thresholds are not supported on multi-currency subscriptions" {
		t.Errorf("error = %+v", env.Error)
	}
}
