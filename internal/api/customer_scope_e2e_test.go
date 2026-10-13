package api

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestE2E_CustomerIDIsTenantAndModeScoped pins that the three operator
// endpoints taking a customer id in the request body — credit grant, credit
// adjust, subscription create — resolve that customer inside the caller's
// tenant and mode before writing anything.
//
// The foreign key from the credit ledger and from subscriptions to customers
// is on the id alone, and foreign-key checks do not apply row-level security,
// so the database accepts a row that points at any existing customer. The
// service's own read is the only thing that scopes the id.
//
// Driven through the real router so it also fails if the router stops wiring
// the customer reader into either service. The same-tenant same-mode control
// keeps a check that refuses everything from passing.
func TestE2E_CustomerIDIsTenantAndModeScoped(t *testing.T) {
	db := testutil.SetupTestDB(t)
	srv := NewServer(db, clock.NewFake(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)), nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	tenantA := testutil.CreateTestTenant(t, db, "Customer Scope A")
	tenantB := testutil.CreateTestTenant(t, db, "Customer Scope B")
	aLive := "Bearer " + createAPIKeyWithMode(t, db, tenantA, true)
	aTest := "Bearer " + createAPIKeyWithMode(t, db, tenantA, false)
	bLive := "Bearer " + createAPIKeyWithMode(t, db, tenantB, true)

	createCustomer := func(auth, externalID string) string {
		t.Helper()
		resp := doPost(t, ts, "/v1/customers", auth, map[string]any{
			"external_id": externalID, "display_name": externalID,
		})
		assertStatus(t, resp, 201)
		return readJSON(t, resp)["id"].(string)
	}
	own := createCustomer(aLive, "cus_scope_own")
	otherTenant := createCustomer(bLive, "cus_scope_other_tenant")
	otherMode := createCustomer(aTest, "cus_scope_other_mode")

	resp := doPost(t, ts, "/v1/plans", aLive, map[string]any{
		"code": "scope-plan", "name": "Scope Plan", "currency": "USD",
		"billing_interval": "monthly", "base_amount_cents": 1000,
	})
	assertStatus(t, resp, 201)
	planID := readJSON(t, resp)["id"].(string)

	// rowsFor counts rows referencing the customer across every tenant and
	// mode, so a row the caller could not read back is still counted.
	rowsFor := func(table, customerID string) int {
		t.Helper()
		tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin bypass tx: %v", err)
		}
		defer postgres.Rollback(tx)
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM `+table+` WHERE customer_id = $1`, customerID).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}

	endpoints := []struct {
		name, path, table string
		body              func(customerID, suffix string) map[string]any
	}{
		{"credit grant", "/v1/credits/grant", "customer_credit_ledger", func(id, _ string) map[string]any {
			return map[string]any{"customer_id": id, "amount_cents": 500, "description": "scope check"}
		}},
		{"credit adjust", "/v1/credits/adjust", "customer_credit_ledger", func(id, _ string) map[string]any {
			return map[string]any{"customer_id": id, "amount_cents": 500, "description": "scope check"}
		}},
		{"subscription create", "/v1/subscriptions", "subscriptions", func(id, suffix string) map[string]any {
			return map[string]any{
				"code": "scope-sub-" + suffix, "display_name": "Scope Sub", "customer_id": id,
				"items": []map[string]any{{"plan_id": planID}},
			}
		}},
	}
	refused := []struct{ name, suffix, customerID string }{
		{"another tenant's customer", "tenant", otherTenant},
		{"same tenant, other mode", "mode", otherMode},
		{"unknown id", "unknown", "vlx_cus_does_not_exist"},
	}

	for _, ep := range endpoints {
		for _, c := range refused {
			t.Run(ep.name+"/"+c.name, func(t *testing.T) {
				before := rowsFor(ep.table, c.customerID)
				resp := doPost(t, ts, ep.path, aLive, ep.body(c.customerID, c.suffix))
				body := readJSON(t, resp)
				if resp.StatusCode != 404 {
					t.Errorf("status = %d, want 404; body: %v", resp.StatusCode, body)
				}
				// One message for all three cases: a customer hidden by
				// isolation reads the same as one that does not exist.
				if errBody, _ := body["error"].(map[string]any); errBody["message"] != "customer not found" {
					t.Errorf("error message = %v, want \"customer not found\"", errBody["message"])
				}
				if wrote := rowsFor(ep.table, c.customerID) - before; wrote != 0 {
					t.Errorf("wrote %d %s row(s) for a customer outside the caller's tenant and mode, want 0", wrote, ep.table)
				}
			})
		}
		t.Run(ep.name+"/own customer", func(t *testing.T) {
			before := rowsFor(ep.table, own)
			resp := doPost(t, ts, ep.path, aLive, ep.body(own, "own"))
			assertStatus(t, resp, 201)
			if wrote := rowsFor(ep.table, own) - before; wrote != 1 {
				t.Errorf("wrote %d %s row(s) for the caller's own customer, want 1", wrote, ep.table)
			}
		})
	}
}
