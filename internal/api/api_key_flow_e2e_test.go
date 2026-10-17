package api

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestE2E_APIKeyCreateUseRevokeRotate walks API keys through the real router:
// create a key and use it, revoke it, and rotate another key with and without
// a grace period, as docs/api-keys.md describes. Before this, the router tests
// minted keys with a database helper, so none of these endpoints was called
// over HTTP and revoke had no success test at any level.
func TestE2E_APIKeyCreateUseRevokeRotate(t *testing.T) {
	f := newFlow(t, nil)
	admin := bearer(f.testKey)

	// Create: the full key is in this response only.
	nameA := "Flow key " + uniq()
	created := f.must(http.StatusCreated, http.MethodPost, "/v1/api-keys", map[string]any{"name": nameA}, admin)
	keyA := obj(created["key"])
	rawA := str(created["raw_key"])
	idA := str(keyA["id"])
	if !strings.HasPrefix(rawA, "vlx_secret_test_") || keyA["key_type"] != "secret" || keyA["livemode"] != false || keyA["name"] != nameA {
		t.Fatalf("created key = %v, raw key %q; want a test-mode secret key named %q", keyA, rawA, nameA)
	}

	// Use: the new key reaches the API, and /whoami names the key, not a user.
	who := f.must(http.StatusOK, http.MethodGet, "/v1/whoami", nil, bearer(rawA))
	if who["key_id"] != idA || who["key_type"] != "secret" || who["livemode"] != false || who["tenant_id"] != f.tenantID {
		t.Errorf("whoami with the new key = %v; want key_id %s, key_type secret, test mode, tenant %s", who, idA, f.tenantID)
	}
	if _, ok := who["user_id"]; ok {
		t.Errorf("whoami with an API key carries user_id: %v", who)
	}
	f.must(http.StatusOK, http.MethodGet, "/v1/customers", nil, bearer(rawA))

	// The key list shows the key, and never its secret.
	list := f.send(http.MethodGet, "/v1/api-keys", nil, admin)
	if list.status != http.StatusOK || findByID(list.body["data"], idA) == nil {
		t.Fatalf("key list: status %d, want the new key listed; body: %s", list.status, list.raw)
	}
	if strings.Contains(string(list.raw), rawA) {
		t.Error("the key list contains the full secret key")
	}

	// Revoke: the key stops working at once.
	revoked := f.must(http.StatusOK, http.MethodDelete, "/v1/api-keys/"+idA, nil, admin)
	if revoked["revoked_at"] == nil {
		t.Errorf("revoke response has no revoked_at: %v", revoked)
	}
	f.must(http.StatusUnauthorized, http.MethodGet, "/v1/customers", nil, bearer(rawA))
	f.must(http.StatusUnauthorized, http.MethodGet, "/v1/whoami", nil, bearer(rawA))
	// A revoked key cannot be rotated back into use.
	if body := f.must(http.StatusConflict, http.MethodPost, "/v1/api-keys/"+idA+"/rotate", nil, admin); errCode(body) != "invalid_state" {
		t.Errorf("rotate a revoked key: code %q, want invalid_state", errCode(body))
	}

	// Rotate with a grace period: the old key and the new key both work
	// until the grace period ends.
	nameB := "Flow rotated key " + uniq()
	createdB := f.must(http.StatusCreated, http.MethodPost, "/v1/api-keys", map[string]any{"name": nameB}, admin)
	idB, rawB := str(obj(createdB["key"])["id"]), str(createdB["raw_key"])
	before := time.Now()
	rot := f.must(http.StatusOK, http.MethodPost, "/v1/api-keys/"+idB+"/rotate", map[string]any{"expires_in_seconds": 3600}, admin)
	oldB, newB, rawB2 := obj(rot["old_key"]), obj(rot["new_key"]), str(rot["raw_key"])
	if oldB["id"] != idB || oldB["revoked_at"] != nil {
		t.Errorf("old key after a 1h rotation = %v; want key %s, not revoked", oldB, idB)
	}
	if exp, err := time.Parse(time.RFC3339, str(oldB["expires_at"])); err != nil ||
		exp.Before(before.Add(time.Hour-time.Minute)) || exp.After(time.Now().Add(time.Hour+time.Minute)) {
		t.Errorf("old key expires_at = %v; want about an hour from now", oldB["expires_at"])
	}
	if newB["id"] == idB || newB["name"] != nameB || newB["key_type"] != "secret" || newB["livemode"] != false ||
		!strings.HasPrefix(rawB2, "vlx_secret_test_") {
		t.Errorf("new key = %v (raw %q); want a new test-mode secret key with the old key's name %q", newB, rawB2, nameB)
	}
	f.must(http.StatusOK, http.MethodGet, "/v1/customers", nil, bearer(rawB))
	f.must(http.StatusOK, http.MethodGet, "/v1/customers", nil, bearer(rawB2))

	// Rotate with no body: the old key stops at once.
	rot2 := f.must(http.StatusOK, http.MethodPost, "/v1/api-keys/"+str(newB["id"])+"/rotate", nil, admin)
	if obj(rot2["old_key"])["revoked_at"] == nil {
		t.Errorf("old key after an immediate rotation = %v; want revoked", rot2["old_key"])
	}
	f.must(http.StatusUnauthorized, http.MethodGet, "/v1/customers", nil, bearer(rawB2))
	f.must(http.StatusOK, http.MethodGet, "/v1/customers", nil, bearer(str(rot2["raw_key"])))
	// The first rotation's old key is still inside its hour.
	f.must(http.StatusOK, http.MethodGet, "/v1/customers", nil, bearer(rawB))

	// Every change wrote its audit row, once.
	for _, want := range []struct{ keyID, action string }{
		{idA, "create"}, {idA, "revoke"}, {idB, "create"},
		{str(newB["id"]), "rotate"}, {str(obj(rot2["new_key"])["id"]), "rotate"},
	} {
		rows := f.must(http.StatusOK, http.MethodGet,
			"/v1/audit-log?resource_type=api_key&resource_id="+want.keyID+"&action="+want.action, nil, admin)
		if n := len(arr(rows["data"])); n != 1 {
			t.Errorf("audit rows for %s on key %s: %d, want 1", want.action, want.keyID, n)
		}
	}
}

// findByID returns the element of a JSON list whose id is id, or nil.
func findByID(list any, id string) map[string]any {
	for _, e := range arr(list) {
		if m := obj(e); m["id"] == id {
			return m
		}
	}
	return nil
}
