package api

import (
	"database/sql"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// A change to a customer through the API runs on the time the API's door
// decided from the customer: on a test clock, its frozen time; at real time,
// the server's clock (the one the server is built with, not the machine's).
// The customer's updated_at and its billing profile's are stamped at that
// time, and the three audit rows (update, billing profile, token rotation)
// carry the clock and its time, or no clock at real time.
func TestE2E_CustomerChangesAreOnTheRequestsTime(t *testing.T) {
	check := func(t *testing.T, r *datesRig, custID, wantClock string, want time.Time) {
		t.Helper()
		at := func(body map[string]any, what string) {
			t.Helper()
			if got, err := time.Parse(time.RFC3339Nano, flowStr(body, "updated_at")); err != nil || !got.Equal(want) {
				t.Errorf("%s updated_at %q (%v); want %s", what, flowStr(body, "updated_at"), err, want)
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
			if wantClock == "" && (clk.Valid || simAt.Valid) {
				t.Errorf("%s row at real time: test_clock_id %v, sim_effective_at %v; want none", action, clk, simAt)
			}
			if wantClock != "" && (clk.String != wantClock || !simAt.Valid || !simAt.Time.Equal(want)) {
				t.Errorf("%s row: test_clock_id %v, sim_effective_at %v; want %s at %s", action, clk, simAt, wantClock, want)
			}
		}
		if n != 3 {
			t.Errorf("%d audit rows; want 3 (update, billing profile, rotation)", n)
		}
	}

	t.Run("on a test clock, at its frozen time", func(t *testing.T) {
		r := newDatesRig(t, "Customer time clock Co")
		frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		clockID := r.newClock("customer time", frozen)
		custID := flowStr(r.call("POST", "/v1/customers", flowReq{body: map[string]any{
			"external_id": "cus-time-clock", "display_name": "Time Co", "test_clock_id": clockID,
		}}).want(t, 201, "create customer on the clock").body, "id")
		check(t, r, custID, clockID, frozen)
	})

	t.Run("at real time, on the server's clock", func(t *testing.T) {
		serverAt := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
		r := newDatesRigOn(t, "Customer time real Co", clock.NewFake(serverAt))
		custID := flowStr(r.call("POST", "/v1/customers", flowReq{body: map[string]any{
			"external_id": "cus-time-real", "display_name": "Time Co",
		}}).want(t, 201, "create customer").body, "id")
		check(t, r, custID, "", serverAt)
	})
}
