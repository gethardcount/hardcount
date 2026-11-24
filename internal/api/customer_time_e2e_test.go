package api

import (
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// A change to a customer through the API runs on the request's instant: one
// read of the server's clock (the one the server is built with, not the
// machine's). The customer's updated_at and its billing profile's are
// stamped at that time, and the three audit rows
// (update, billing profile, token rotation) are written.
func TestE2E_CustomerChangesAreOnTheRequestsTime(t *testing.T) {
	serverAt := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	r := newDatesRigOn(t, "Customer time real Co", clock.NewFake(serverAt))
	custID := flowStr(r.call("POST", "/v1/customers", flowReq{body: map[string]any{
		"external_id": "cus-time-real", "display_name": "Time Co",
	}}).want(t, 201, "create customer").body, "id")

	at := func(body map[string]any, what string) {
		t.Helper()
		if got, err := time.Parse(time.RFC3339Nano, flowStr(body, "updated_at")); err != nil || !got.Equal(serverAt) {
			t.Errorf("%s updated_at %q (%v); want %s", what, flowStr(body, "updated_at"), err, serverAt)
		}
	}
	at(r.call("PATCH", "/v1/customers/"+custID, flowReq{body: map[string]any{"display_name": "Renamed Co"}}).want(t, 200, "update").body, "customer")
	at(r.call("PUT", "/v1/customers/"+custID+"/billing-profile", flowReq{body: map[string]any{"legal_name": "Renamed Co"}}).want(t, 200, "billing profile").body, "billing profile")
	r.call("POST", "/v1/customers/"+custID+"/rotate-cost-dashboard-token", flowReq{}).want(t, 200, "rotate")

	tx, err := r.db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(t.Context(), `
		SELECT count(*) FROM audit_log
		WHERE tenant_id = $1 AND resource_type = 'customer' AND resource_id = $2 AND action IN ('update', 'rotate')`,
		r.tenantID, custID).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if n != 3 {
		t.Errorf("%d audit rows; want 3 (update, billing profile, rotation)", n)
	}
}
