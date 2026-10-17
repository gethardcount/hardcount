package api

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestE2E_DunningPolicyDelete walks "Delete policy" on the dunning policies
// page over the real router and database. A policy is refused while it is
// the default or while a customer is assigned to it, and deleted once
// neither holds. Before this test the delete had no success test at any
// level, only a stub store.
func TestE2E_DunningPolicyDelete(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Dunning Delete Flow Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	call := func(method, path string, req flowReq) flowResp {
		t.Helper()
		req.bearer = key
		return flowDo(t, ts, method, path, req)
	}
	newPolicy := func(name string) map[string]any {
		t.Helper()
		return call("POST", "/v1/dunning/policies", flowReq{body: map[string]any{
			"name": name, "enabled": true, "max_retry_attempts": 2, "retry_schedule": []string{"72h", "120h"},
		}}).want(t, 201, "create policy "+name).body
	}
	// assigned lists the policies with their "N customers assigned" count,
	// as the policies page shows them.
	assigned := func() map[string]int64 {
		t.Helper()
		out := map[string]int64{}
		for _, p := range flowList(call("GET", "/v1/dunning/policies", flowReq{}).want(t, 200, "list policies").body, "data") {
			out[flowStr(p, "id")] = flowInt(p, "assigned_customers")
		}
		return out
	}

	standard := newPolicy("Standard (delete flow)")
	strict := newPolicy("Strict (delete flow)")
	standardID, strictID := flowStr(standard, "id"), flowStr(strict, "id")
	if standard["is_default"] != true || strict["is_default"] != false {
		t.Fatalf("first policy default=%v, second default=%v; want the first to be the default", standard["is_default"], strict["is_default"])
	}

	// The default cannot be deleted.
	if res := call("DELETE", "/v1/dunning/policies/"+standardID, flowReq{}).want(t, 409, "delete the default policy"); !strings.Contains(res.errMessage(), "default") {
		t.Errorf("default delete message %q, want it to name the default", res.errMessage())
	}

	// Nor can a policy a customer is on.
	customerID := flowStr(call("POST", "/v1/customers", flowReq{body: map[string]any{
		"external_id": "dunning-delete-flow", "display_name": "Dunning Delete Customer", "email": "ap@dunning-delete-flow.test",
	}}).want(t, 201, "create customer").body, "id")
	call("PATCH", "/v1/customers/"+customerID, flowReq{body: map[string]any{"dunning_policy_id": strictID}}).want(t, 200, "assign the customer")
	if n := assigned()[strictID]; n != 1 {
		t.Fatalf("strict policy shows %d assigned customers, want 1", n)
	}
	if res := call("DELETE", "/v1/dunning/policies/"+strictID, flowReq{}).want(t, 409, "delete an assigned policy"); !strings.Contains(res.errMessage(), "1 customer(s) still assigned") {
		t.Errorf("assigned delete message %q, want it to count the assigned customer", res.errMessage())
	}
	call("GET", "/v1/dunning/policies/"+strictID, flowReq{}).want(t, 200, "the refused policy is still there")

	// Move the customer back to the default, and the delete goes through.
	moved := call("PATCH", "/v1/customers/"+customerID, flowReq{body: map[string]any{"dunning_policy_id": ""}}).want(t, 200, "unassign the customer")
	if got := flowStr(moved.body, "dunning_policy_id"); got != "" {
		t.Fatalf("customer still on policy %q after clearing it", got)
	}
	if res := call("DELETE", "/v1/dunning/policies/"+strictID, flowReq{}).want(t, 200, "delete the policy"); flowStr(res.body, "status") != "deleted" {
		t.Errorf("delete answered %s, want status deleted", res.raw)
	}
	call("GET", "/v1/dunning/policies/"+strictID, flowReq{}).want(t, 404, "get the deleted policy")
	call("DELETE", "/v1/dunning/policies/"+strictID, flowReq{}).want(t, 404, "delete it again")
	left := assigned()
	if _, ok := left[strictID]; ok || len(left) != 1 {
		t.Errorf("policies after the delete: %v, want only the default %s", left, standardID)
	}
	if _, ok := left[standardID]; !ok {
		t.Errorf("the default policy %s is gone after deleting another one", standardID)
	}

	// The delete is in the audit log.
	var actions []string
	for _, e := range flowList(call("GET", "/v1/audit-log?resource_type=dunning_policy&resource_id="+strictID, flowReq{}).want(t, 200, "audit log").body, "data") {
		actions = append(actions, flowStr(e, "action"))
	}
	if strings.Count(","+strings.Join(actions, ",")+",", ",delete,") != 1 {
		t.Errorf("audit rows for the deleted policy: %v, want one delete", actions)
	}
}
