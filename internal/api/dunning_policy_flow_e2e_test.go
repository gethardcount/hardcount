package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestE2E_DunningPolicyDelete deletes dunning policies through the router on
// a real database: refused while a customer is assigned to the policy or
// while it is the default, deleted once the customer moves off it. Before
// this, the delete existed only as an in-memory store stub in tests.
func TestE2E_DunningPolicyDelete(t *testing.T) {
	f := newFlow(t, nil)
	admin := bearer(f.testKey)

	// A new account has one seeded default policy (ADR-123).
	seeded := f.must(http.StatusOK, http.MethodGet, "/v1/dunning/policies", nil, admin)["data"]
	if len(arr(seeded)) != 1 || obj(arr(seeded)[0])["is_default"] != true || obj(arr(seeded)[0])["seeded"] != true {
		t.Fatalf("policies on a new account = %v; want one seeded default", seeded)
	}
	seededID := str(obj(arr(seeded)[0])["id"])

	// The first policy the operator adds takes over as the default; the
	// second does not.
	standard := f.must(http.StatusCreated, http.MethodPost, "/v1/dunning/policies", map[string]any{
		"name": "Flow standard " + uniq(), "enabled": true, "max_retry_attempts": 2, "retry_schedule": []string{"72h"},
	}, admin)
	strict := f.must(http.StatusCreated, http.MethodPost, "/v1/dunning/policies", map[string]any{
		"name": "Flow strict " + uniq(), "enabled": true, "max_retry_attempts": 1,
	}, admin)
	standardID, strictID := str(standard["id"]), str(strict["id"])
	if standard["is_default"] != true || strict["is_default"] != false {
		t.Fatalf("is_default: standard %v, strict %v; want true, false", standard["is_default"], strict["is_default"])
	}

	cust := f.customer()
	custID := str(cust["id"])
	if c := f.must(http.StatusOK, http.MethodPatch, "/v1/customers/"+custID, map[string]any{"dunning_policy_id": strictID}, admin); c["dunning_policy_id"] != strictID {
		t.Fatalf("assign policy: customer = %v", c)
	}
	if p := findByID(f.must(http.StatusOK, http.MethodGet, "/v1/dunning/policies", nil, admin)["data"], strictID); num(p["assigned_customers"]) != 1 {
		t.Errorf("strict policy in the list = %v; want 1 assigned customer", p)
	}

	// Refused while a customer is on it, and for the default.
	refused := f.must(http.StatusConflict, http.MethodDelete, "/v1/dunning/policies/"+strictID, nil, admin)
	if errCode(refused) != "invalid_state" || !strings.Contains(str(obj(refused["error"])["message"]), "1 customer(s) still assigned") {
		t.Errorf("delete an assigned policy: %v; want invalid_state naming the 1 assigned customer", refused)
	}
	if body := f.must(http.StatusConflict, http.MethodDelete, "/v1/dunning/policies/"+standardID, nil, admin); errCode(body) != "invalid_state" {
		t.Errorf("delete the default policy: %v; want invalid_state", body)
	}

	// Move the customer back to the default: now the policy deletes.
	f.must(http.StatusOK, http.MethodPatch, "/v1/customers/"+custID, map[string]any{"dunning_policy_id": ""}, admin)
	if body := f.must(http.StatusOK, http.MethodDelete, "/v1/dunning/policies/"+strictID, nil, admin); body["status"] != "deleted" {
		t.Errorf("delete: %v", body)
	}
	f.must(http.StatusNotFound, http.MethodGet, "/v1/dunning/policies/"+strictID, nil, admin)
	f.must(http.StatusNotFound, http.MethodDelete, "/v1/dunning/policies/"+strictID, nil, admin)

	// The seeded policy, no longer the default, deletes too.
	f.must(http.StatusOK, http.MethodDelete, "/v1/dunning/policies/"+seededID, nil, admin)
	left := f.must(http.StatusOK, http.MethodGet, "/v1/dunning/policies", nil, admin)["data"]
	if len(arr(left)) != 1 || findByID(left, standardID) == nil || obj(arr(left)[0])["is_default"] != true {
		t.Errorf("policies after the deletes = %v; want only the standard policy, as the default", left)
	}
	for _, id := range []string{strictID, seededID} {
		rows := f.must(http.StatusOK, http.MethodGet, "/v1/audit-log?resource_type=dunning_policy&action=delete&resource_id="+id, nil, admin)
		if n := len(arr(rows["data"])); n != 1 {
			t.Errorf("delete audit rows for policy %s: %d, want 1", id, n)
		}
	}
}
