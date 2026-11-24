package api

import (
	"database/sql"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// A change to a customer through the API runs on the time the API's door
// decided from the customer: at real time, the server's clock (the one the
// server is built with, not the machine's). The customer's updated_at and its
// billing profile's are stamped at that time, and the three audit rows
// (update, billing profile, token rotation) carry no clock.
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
	rows, err := tx.QueryContext(t.Context(), `
		SELECT action, sim_effective_at, test_clock_id FROM audit_log
		WHERE tenant_id = $1 AND resource_type = 'customer' AND resource_id = $2 AND action IN ('update', 'rotate')
		ORDER BY created_at, id`, r.tenantID, custID)
	if err != nil {
		t.Fatalf("read audit rows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var (
			action string
			simAt  sql.NullTime
			clk    sql.NullString
		)
		if err := rows.Scan(&action, &simAt, &clk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n++
		if clk.Valid || simAt.Valid {
			t.Errorf("%s row at real time: test_clock_id %v, sim_effective_at %v; want none", action, clk, simAt)
		}
	}
	if n != 3 {
		t.Errorf("%d audit rows; want 3 (update, billing profile, rotation)", n)
	}
}
