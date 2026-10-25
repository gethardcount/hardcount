package api

import (
	"context"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// TestRemovedRoutes_AnswerNotFound pins the operations removed after v0.8.2
// (simplification audit B2). Each one had a kept operation that does
// the same job, so it was removed rather than kept as a second way in.
//
// Each case calls the removed operation with ids that exist, so a handler
// that is still mounted would answer it, and checks that the router answers
// 404 itself: the plain-text "404 page not found", not a handler's JSON 404.
// Each case also calls the kept replacement with the same ids and expects it
// to answer, so a router that refuses everything cannot pass.
func TestRemovedRoutes_AnswerNotFound(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Removed Routes Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	call := func(method, path string, body any) flowResp {
		t.Helper()
		return flowDo(t, ts, method, path, flowReq{bearer: key, body: body})
	}

	ruleID := flowStr(call("POST", "/v1/rating-rules", map[string]any{
		"rule_key": "removed_calls", "name": "Removed calls", "mode": "per_unit",
		"currency": "USD", "unit_amount_cents": 10,
	}).want(t, 201, "create rating rule").body, "id")
	meterID := flowStr(call("POST", "/v1/meters", map[string]any{
		"event_name": "removed_calls", "name": "Removed calls", "unit": "call",
		"aggregation": "sum", "rating_rule_version_id": ruleID,
	}).want(t, 201, "create meter").body, "id")
	planID := flowStr(call("POST", "/v1/plans", map[string]any{
		"code": "removed-plan", "name": "Removed Plan", "currency": "USD",
		"billing_interval": "monthly", "base_amount_cents": 1000,
		"base_bill_timing": "in_arrears", "meter_ids": []string{meterID},
	}).want(t, 201, "create plan").body, "id")
	customerID := flowStr(call("POST", "/v1/customers", map[string]any{
		"external_id": "removed-cust", "display_name": "Removed Routes Customer",
		"email": "billing@removed.test",
	}).want(t, 201, "create customer").body, "id")
	subID := flowStr(call("POST", "/v1/subscriptions", map[string]any{
		"code": "removed-sub", "display_name": "Removed Routes Subscription", "customer_id": customerID,
		"items":        []map[string]any{{"plan_id": planID, "quantity": 1}},
		"billing_time": "anniversary",
	}).want(t, 201, "create subscription").body, "id")

	// An outbound event for the two webhook cases. Events reach the table
	// through the outbox worker, which this server does not run, so the
	// test writes one the way the webhook package's own tests do.
	event, err := webhook.NewPostgresStore(db).CreateEvent(postgres.WithLivemode(context.Background(), false), tenantID,
		domain.WebhookEvent{EventType: "customer.created", Payload: map[string]any{"customer_id": customerID}})
	if err != nil {
		t.Fatalf("create webhook event: %v", err)
	}
	eventID := event.ID

	cases := []struct {
		name             string
		method, path     string
		body             any
		keptMethod, kept string
		keptBody         any
		// keptNeedsStripe: the kept operation calls Stripe, which this test
		// does not connect, so it answers an error instead of 2xx.
		keptNeedsStripe bool
	}{
		{
			name:   "legacy checkout setup",
			method: "POST", path: "/v1/checkout/setup",
			body:       map[string]any{"customer_id": customerID},
			keptMethod: "POST", kept: "/v1/customers/" + customerID + "/payment-methods/setup-session",
			keptBody: map[string]any{}, keptNeedsStripe: true,
		},
		{
			name:   "legacy checkout status",
			method: "GET", path: "/v1/checkout/status/" + customerID,
			keptMethod: "GET", kept: "/v1/customers/" + customerID + "/payment-methods",
		},
		{
			name:   "legacy event deliveries",
			method: "GET", path: "/v1/webhook-endpoints/events/" + eventID + "/deliveries",
			keptMethod: "GET", kept: "/v1/webhook_events/" + eventID + "/deliveries",
		},
		{
			name:   "legacy event replay",
			method: "POST", path: "/v1/webhook-endpoints/events/" + eventID + "/replay",
			body:       map[string]any{},
			keptMethod: "POST", kept: "/v1/webhook_events/" + eventID + "/replay",
			keptBody: map[string]any{},
		},
		{
			name:   "customer invoice sub-list",
			method: "GET", path: "/v1/customers/" + customerID + "/invoices",
			keptMethod: "GET", kept: "/v1/invoices?customer_id=" + customerID,
		},
		{
			name:   "customer subscription sub-list",
			method: "GET", path: "/v1/customers/" + customerID + "/subscriptions",
			keptMethod: "GET", kept: "/v1/subscriptions?customer_id=" + customerID,
		},
		{
			name:   "usage summary",
			method: "GET", path: "/v1/usage-summary/" + customerID,
			keptMethod: "GET", kept: "/v1/usage-events/aggregate?customer_id=" + customerID,
		},
		{
			name:   "billing preview",
			method: "GET", path: "/v1/billing/preview/" + subID,
			keptMethod: "POST", kept: "/v1/invoices/create_preview",
			keptBody: map[string]any{"customer_id": customerID, "subscription_id": subID},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := call(tc.method, tc.path, tc.body)
			if got.status != 404 || !strings.Contains(string(got.raw), "404 page not found") {
				t.Errorf("%s %s = %d %q, want the router's 404: the operation was removed", tc.method, tc.path, got.status, got.raw)
			}
			kept := call(tc.keptMethod, tc.kept, tc.keptBody)
			switch {
			case tc.keptNeedsStripe:
				// A handler answered with the JSON error envelope, which the
				// router's plain-text 404 and empty 405 never carry.
				if kept.status == 404 || kept.status == 405 || kept.errCode() == "" {
					t.Errorf("kept %s %s = %d %s, want its handler's error", tc.keptMethod, tc.kept, kept.status, kept.raw)
				}
			case kept.status/100 != 2:
				t.Errorf("kept %s %s = %d %s, want 2xx", tc.keptMethod, tc.kept, kept.status, kept.raw)
			}
		})
	}
}

// TestKeptEventDeliveries_AnswersTheCHANGELOGNames pins the two answers of
// GET /v1/webhook_events/{id}/deliveries that the removed
// GET /v1/webhook-endpoints/events/{id}/deliveries did not give, which the
// CHANGELOG's Removed entry names. The removed route listed deliveries for
// the id as given, with no event lookup:
//   - an unknown id, or another tenant's, answered 200 with an empty list;
//     the kept route answers 404;
//   - a replay's id listed only that replay's deliveries; the kept route
//     answers for the original event and every replay of it.
func TestKeptEventDeliveries_AnswersTheCHANGELOGNames(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Kept Deliveries Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	otherTenantID := testutil.CreateTestTenant(t, db, "Kept Deliveries Other Co")

	ctx := postgres.WithLivemode(context.Background(), false)
	store := webhook.NewPostgresStore(db)
	ep, err := store.CreateEndpoint(ctx, tenantID, domain.WebhookEndpoint{
		URL: "https://example.test/hook", Events: []string{"*"}, Active: true, Secret: "whsec_test",
	})
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	root, err := store.CreateEvent(ctx, tenantID, domain.WebhookEvent{EventType: "customer.created", Payload: map[string]any{"customer_id": "vlx_cus_1"}})
	if err != nil {
		t.Fatalf("create event: %v", err)
	}
	rootDeliveries, err := store.CreateDeliveriesForEvent(ctx, tenantID, root.ID, []string{ep.ID}, time.Minute)
	if err != nil {
		t.Fatalf("create root delivery: %v", err)
	}
	clone, err := store.CreateReplayEvent(ctx, tenantID, root.ID)
	if err != nil {
		t.Fatalf("create replay: %v", err)
	}
	cloneDeliveries, err := store.CreateDeliveriesForEvent(ctx, tenantID, clone.ID, []string{ep.ID}, time.Minute)
	if err != nil {
		t.Fatalf("create replay delivery: %v", err)
	}
	otherEvent, err := store.CreateEvent(ctx, otherTenantID, domain.WebhookEvent{EventType: "customer.created", Payload: map[string]any{}})
	if err != nil {
		t.Fatalf("create other tenant's event: %v", err)
	}

	get := func(id string) flowResp {
		t.Helper()
		return flowDo(t, ts, "GET", "/v1/webhook_events/"+id+"/deliveries", flowReq{bearer: key})
	}

	for name, id := range map[string]string{
		"unknown id":             "vlx_whe_doesnotexist",
		"another tenant's event": otherEvent.ID,
	} {
		t.Run(name, func(t *testing.T) {
			got := get(id)
			if got.status != 404 || got.errCode() != "not_found" {
				t.Errorf("GET deliveries for %s = %d %s, want 404 not_found", name, got.status, got.raw)
			}
		})
	}

	t.Run("a replay's id answers for the whole chain", func(t *testing.T) {
		got := get(clone.ID).want(t, 200, "deliveries for a replay's id")
		if rootID := flowStr(got.body, "root_event_id"); rootID != root.ID {
			t.Errorf("root_event_id = %q, want the original event %q", rootID, root.ID)
		}
		rows, _ := got.body["deliveries"].([]any)
		var ids []string
		for _, r := range rows {
			row, _ := r.(map[string]any)
			ids = append(ids, flowStr(row, "id"))
		}
		want := []string{rootDeliveries[0].ID, cloneDeliveries[0].ID}
		if !slices.Equal(ids, want) {
			t.Errorf("delivery ids = %v, want the original's then the replay's %v", ids, want)
		}
	})
}
