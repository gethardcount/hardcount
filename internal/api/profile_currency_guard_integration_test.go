package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestBillingProfileCurrency_ChecksEveryLiveSubscription: the billing-profile
// currency guard reads every live subscription of the customer, however many
// there are. It read at most 100 of each status, newest first. So a customer
// with 101 active subscriptions could take a profile currency that the oldest
// one does not bill in, and that subscription's invoices would show its plan
// prices in the profile currency without conversion (ADR-100).
//
// The subscriptions are written with SQL: the currency pin refuses a second
// currency on one customer at create (ADR-100 G3), so two currencies on one
// customer exist only in rows written before the customer-rules lock closed
// the race between two creates. The guard must still see them.
//
// The EUR subscription is both the oldest and the last by code, so a page of
// 100 in either order misses it: the old read sorted newest first, and the
// query the guard uses now sorts by code.
//
// Mutation-verify: add LIMIT 100 to CustomerSubPlanCurrencies → 200, and the
// USD profile is saved.
func TestBillingProfileCurrency_ChecksEveryLiveSubscription(t *testing.T) {
	e := newCcyEnv(t, "Profile Currency Guard Co")
	cus := e.customer(t, "guard-101")
	plan := func(code, currency string) string {
		return flowStr(e.call(t, "POST", "/v1/plans", map[string]any{
			"code": code, "name": code, "currency": currency, "billing_interval": "monthly",
			"base_amount_cents": 1000, "base_bill_timing": "in_arrears",
		}).want(t, 201, "create plan "+code).body, "id")
	}
	usd, eur := plan("guard-usd", "USD"), plan("guard-eur", "EUR")

	// The EUR subscription is the oldest, so a read of the newest 100 misses
	// it. Its code sorts after every 'guard-usd-N', so a read of the first 100
	// by code misses it too.
	seedBypass(t, e.db, `WITH s AS (
			INSERT INTO subscriptions (tenant_id, livemode, customer_id, code, display_name, status, created_at)
			VALUES ($1, false, $2, 'zz-guard-eur-sub', 'Guard EUR', 'active', now() - interval '1 day')
			RETURNING id)
		INSERT INTO subscription_items (tenant_id, livemode, subscription_id, plan_id)
		SELECT $1, false, s.id, $3 FROM s`, e.tenantID, cus, eur)
	seedBypass(t, e.db, `WITH s AS (
			INSERT INTO subscriptions (tenant_id, livemode, customer_id, code, display_name, status)
			SELECT $1, false, $2, 'guard-usd-' || g, 'Guard USD ' || g, 'active' FROM generate_series(1, 100) g
			RETURNING id)
		INSERT INTO subscription_items (tenant_id, livemode, subscription_id, plan_id)
		SELECT $1, false, s.id, $3 FROM s`, e.tenantID, cus, usd)

	r := e.call(t, "PUT", "/v1/customers/"+cus+"/billing-profile", map[string]any{
		"legal_name": "Guard Co", "currency": "USD",
	})
	if r.status != http.StatusConflict || r.errCode() != "invalid_state" {
		t.Fatalf("USD profile over 100 USD subscriptions and 1 EUR: status %d code %q, want 409 invalid_state; body: %s", r.status, r.errCode(), r.raw)
	}
	if !strings.Contains(r.errMessage(), "zz-guard-eur-sub") {
		t.Errorf("409 message %q does not name the EUR subscription zz-guard-eur-sub", r.errMessage())
	}
	if got := e.call(t, "GET", "/v1/customers/"+cus+"/billing-profile", nil); flowStr(got.body, "currency") == "USD" {
		t.Errorf("the refused profile was saved: %s", got.raw)
	}
}

// TestBillingProfileCurrency_CountsTrialingAndDraftSubscriptions: a trialing
// or a draft subscription will bill, so its plan's currency blocks a profile
// currency it does not bill in, as an active one does.
func TestBillingProfileCurrency_CountsTrialingAndDraftSubscriptions(t *testing.T) {
	for _, status := range []string{"trialing", "draft"} {
		t.Run(status, func(t *testing.T) {
			e := newCcyEnv(t, "Profile Currency Guard "+status)
			cus := e.customer(t, "guard-"+status)
			eur := flowStr(e.call(t, "POST", "/v1/plans", map[string]any{
				"code": "guard-eur-" + status, "name": "guard-eur", "currency": "EUR", "billing_interval": "monthly",
				"base_amount_cents": 1000, "base_bill_timing": "in_arrears",
			}).want(t, 201, "create plan").body, "id")
			seedBypass(t, e.db, `WITH s AS (
					INSERT INTO subscriptions (tenant_id, livemode, customer_id, code, display_name, status)
					VALUES ($1, false, $2, 'guard-eur-sub', 'Guard EUR', $4)
					RETURNING id)
				INSERT INTO subscription_items (tenant_id, livemode, subscription_id, plan_id)
				SELECT $1, false, s.id, $3 FROM s`, e.tenantID, cus, eur, status)

			r := e.call(t, "PUT", "/v1/customers/"+cus+"/billing-profile", map[string]any{
				"legal_name": "Guard Co", "currency": "USD",
			})
			if r.status != http.StatusConflict || r.errCode() != "invalid_state" {
				t.Fatalf("USD profile over a %s EUR subscription: status %d code %q, want 409 invalid_state; body: %s", status, r.status, r.errCode(), r.raw)
			}
		})
	}
}
