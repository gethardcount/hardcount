package api

import (
	"net/http/httptest"
	"testing"

	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TEMPORARY probe (not for merge): deleting a dunning policy that once ran a
// campaign. invoice_dunning_runs.policy_id is ON DELETE RESTRICT and the
// delete path does not check runs, so the expectation is a 500.
func TestE2E_ProbeDunningPolicyDeleteAfterCampaign(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()
	tenantID := testutil.CreateTestTenant(t, db, "Probe Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	call := func(method, path string, req flowReq) flowResp {
		t.Helper()
		req.bearer = key
		return flowDo(t, ts, method, path, req)
	}
	policy := func(name string) string {
		return flowStr(call("POST", "/v1/dunning/policies", flowReq{body: map[string]any{
			"name": name, "enabled": true, "max_retry_attempts": 2, "retry_schedule": []string{"72h", "120h"},
		}}).want(t, 201, "policy").body, "id")
	}
	_ = policy("Default")
	campaign := policy("Campaign")
	cus := flowStr(call("POST", "/v1/customers", flowReq{body: map[string]any{
		"external_id": "probe", "display_name": "Probe", "email": "probe@probe.test",
	}}).want(t, 201, "customer").body, "id")
	call("PATCH", "/v1/customers/"+cus, flowReq{body: map[string]any{"dunning_policy_id": campaign}}).want(t, 200, "assign")
	inv := flowStr(call("POST", "/v1/invoices", flowReq{body: map[string]any{
		"customer_id": cus, "line_items": []map[string]any{{"description": "x", "quantity": 1, "unit_amount_cents": 1000}},
	}}).want(t, 201, "invoice").body, "id")
	call("POST", "/v1/invoices/"+inv+"/finalize", flowReq{}).want(t, 200, "finalize")
	runs := flowList(call("GET", "/v1/dunning/runs?invoice_id="+inv, flowReq{}).want(t, 200, "runs").body, "data")
	if len(runs) != 1 || flowStr(runs[0], "policy_id") != campaign {
		t.Fatalf("runs: %v, want one run on the campaign policy %s", runs, campaign)
	}
	call("POST", "/v1/invoices/"+inv+"/record-payment", flowReq{}).want(t, 200, "record payment")
	t.Logf("run after payment: %v", flowList(call("GET", "/v1/dunning/runs?invoice_id="+inv, flowReq{}).body, "data"))
	call("PATCH", "/v1/customers/"+cus, flowReq{body: map[string]any{"dunning_policy_id": ""}}).want(t, 200, "unassign")
	res := call("DELETE", "/v1/dunning/policies/"+campaign, flowReq{})
	t.Logf("DELETE policy that ran a resolved campaign, no customers assigned: %d %s", res.status, res.raw)
	if res.status >= 500 {
		t.Errorf("BUG: DELETE /v1/dunning/policies/{id} on a policy with a past dunning run answered %d %s", res.status, res.raw)
	}
}
