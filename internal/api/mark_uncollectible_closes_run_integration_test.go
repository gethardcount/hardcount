package api

import (
	"net/http/httptest"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// The operator's write-off (POST /v1/invoices/{id}/mark-uncollectible) closes
// the invoice's dunning run as invoice_not_collectible and queues
// dunning.resolved once: an active run, and an escalated run on an invoice
// not yet written off (a policy whose final action leaves the invoice open).
func TestMarkUncollectible_ClosesTheInvoicesRun(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	t.Cleanup(ts.Close)
	tenantID := testutil.CreateTestTenant(t, db, "Write-off Closes Run")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	call := func(method, path string, body any) flowResp {
		t.Helper()
		return flowDo(t, ts, method, path, flowReq{bearer: key, body: body})
	}
	// A dunning policy, so finalizing an invoice with no card opens a run.
	call("PUT", "/v1/dunning/policy", map[string]any{
		"final_subscription_action": "pause", "final_invoice_action": "none",
		"enabled": true, "retry_days": []int{3, 3},
	}).want(t, 200, "create dunning policy")
	customerID := flowStr(call("POST", "/v1/customers", map[string]any{
		"external_id": "write-off-closes", "display_name": "Write-off Closes",
	}).want(t, 201, "create customer").body, "id")
	dunned := func(desc string) (invoiceID, runID string) {
		t.Helper()
		invoiceID = flowStr(call("POST", "/v1/invoices", map[string]any{
			"customer_id": customerID,
			"line_items":  []map[string]any{{"description": desc, "quantity": 1, "unit_amount_cents": 3100}},
		}).want(t, 201, "create invoice").body, "id")
		call("POST", "/v1/invoices/"+invoiceID+"/finalize", nil).want(t, 200, "finalize")
		runs := flowList(call("GET", "/v1/dunning/runs?invoice_id="+invoiceID, nil).want(t, 200, "list runs").body, "data")
		if len(runs) != 1 {
			t.Fatalf("invoice %s has %d dunning runs, want 1", invoiceID, len(runs))
		}
		return invoiceID, flowStr(runs[0], "id")
	}
	check := func(t *testing.T, runID string) {
		t.Helper()
		run := flowObj(call("GET", "/v1/dunning/runs/"+runID, nil).want(t, 200, "get run").body, "run")
		if flowStr(run, "state") != "resolved" || flowStr(run, "resolution") != "invoice_not_collectible" {
			t.Errorf("run state=%s resolution=%s, want resolved / invoice_not_collectible", flowStr(run, "state"), flowStr(run, "resolution"))
		}
		if n := flowCount(t, db, `SELECT count(*) FROM webhook_outbox WHERE event_type = 'dunning.resolved' AND payload->'dunning_run'->>'id' = $1`, runID); n != 1 {
			t.Errorf("dunning.resolved queued %d times, want 1", n)
		}
	}

	t.Run("an active run", func(t *testing.T) {
		invoiceID, runID := dunned("Active run")
		call("POST", "/v1/invoices/"+invoiceID+"/mark-uncollectible", nil).want(t, 200, "mark uncollectible")
		check(t, runID)
	})

	t.Run("an escalated run on an invoice not yet written off", func(t *testing.T) {
		invoiceID, runID := dunned("Escalated run")
		// Escalated as a policy with no invoice action leaves it: retries
		// exhausted, the invoice still open.
		tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.ExecContext(t.Context(), `UPDATE invoice_dunning_runs
			SET state = 'escalated', resolution = 'retries_exhausted', resolved_at = now(), next_action_at = NULL
			WHERE id = $1`, runID); err != nil {
			postgres.Rollback(tx)
			t.Fatalf("escalate run: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		call("POST", "/v1/invoices/"+invoiceID+"/mark-uncollectible", nil).want(t, 200, "mark uncollectible")
		check(t, runID)
	})
}
