package subscription

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
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

// These handlers used to pass respond.Error its arguments shifted by one:
// the message text landed in `code`, the field name in `message`, and a
// made-up word ("invalid_body", "validation_error") in `type`. A client
// switching on type or code could not recognise the error.
func TestExtendTrial_ErrorEnvelope(t *testing.T) {
	store := newMemStore()
	subID, _ := seedSubWithItem(t, store, "t1", "cus_1", "plan_pro")
	h := NewHandler(NewService(store, nil))

	cases := []struct {
		name                     string
		body                     string
		status                   int
		wantType, wantCode, want string // want = param
	}{
		{"malformed JSON", `{not json`, http.StatusBadRequest, "invalid_request_error", "invalid_request", ""},
		{"missing trial_end", `{}`, http.StatusUnprocessableEntity, "invalid_request_error", "validation_error", "trial_end"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := withTenantRoute(httptest.NewRequest(http.MethodPost, "/subscriptions/"+subID+"/extend-trial", bytes.NewBufferString(tc.body)), "t1", map[string]string{"id": subID})
			rr := httptest.NewRecorder()
			h.extendTrial(rr, req)
			if rr.Code != tc.status {
				t.Fatalf("status = %d, want %d; body=%s", rr.Code, tc.status, rr.Body.String())
			}
			env := decodeEnvelope(t, rr)
			if env.Error.Type != tc.wantType || env.Error.Code != tc.wantCode || env.Error.Param != tc.want || env.Error.Message == "" {
				t.Errorf("error = %+v, want type=%s code=%s param=%q and a message", env.Error, tc.wantType, tc.wantCode, tc.want)
			}
		})
	}
}

func TestSetBillingThresholds_MultiCurrencyErrorEnvelope(t *testing.T) {
	store := newMemStore()
	now := time.Now().UTC()
	start, end := now.Add(-15*24*time.Hour), now.Add(15*24*time.Hour)
	sub, err := store.Create(context.Background(), "t1", domain.Subscription{
		Code: "sub-mc", CustomerID: "cus_1",
		Status:                    domain.SubscriptionActive,
		BillingTime:               domain.BillingTimeCalendar,
		CurrentBillingPeriodStart: &start,
		CurrentBillingPeriodEnd:   &end,
		Items: []domain.SubscriptionItem{
			{PlanID: "plan_usd", Quantity: 1},
			{PlanID: "plan_eur", Quantity: 1},
		},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	h := NewHandler(NewService(store, nil))
	h.SetProrationDeps(&plansMock{plans: map[string]domain.Plan{
		"plan_usd": {ID: "plan_usd", Currency: "USD"},
		"plan_eur": {ID: "plan_eur", Currency: "EUR"},
	}}, &invoicesMock{}, &creditsMock{})

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
