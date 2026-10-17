package api

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestE2E_APIKeyLifecycle walks the API keys page over the real router and
// database: create a key from a signed-in session, use it, find it in the
// list, revoke it and see the same key refused, then rotate the two ways
// docs/api-keys.md describes. A grace period keeps the old key working;
// no body retires it at once. Before this test no test called these routes
// over HTTP: every other test mints its key with a database helper.
func TestE2E_APIKeyLifecycle(t *testing.T) {
	db := testutil.SetupTestDB(t)
	t.Setenv("VELOX_BOOTSTRAP_TOKEN", "apikey-flow-bootstrap")
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	const email, password = "owner@apikey-flow.test", "apikey-flow-pw-12"
	boot := flowBootstrap(t, ts, "apikey-flow-bootstrap", email, password)
	tenantID := flowStr(boot, "tenant", "id")
	owner := login(t, ts, email, password)

	createKey := func(name, keyType string) (id, raw string) {
		t.Helper()
		res := flowDo(t, ts, "POST", "/v1/api-keys", flowReq{cookie: owner, body: map[string]any{
			"name": name, "key_type": keyType,
		}}).want(t, 201, "create "+name)
		if got := flowObj(res.body, "key"); got["livemode"] != false || flowStr(got, "name") != name || flowStr(got, "key_type") != keyType {
			t.Fatalf("created key: %v, want a test-mode %s key named %q", got, keyType, name)
		}
		return flowStr(res.body, "key", "id"), flowStr(res.body, "raw_key")
	}
	readCustomers := func(key string) int {
		t.Helper()
		return flowDo(t, ts, "GET", "/v1/customers", flowReq{bearer: key}).status
	}

	// Create: the full key comes back once, in the session's mode.
	keyID, rawKey := createKey("CI server", "secret")
	if !strings.HasPrefix(rawKey, "vlx_secret_test_") {
		t.Fatalf("raw key %q: want a vlx_secret_test_ key (the session is in test mode)", rawKey)
	}

	// Use: whoami names this key and this account, and a read works.
	who := flowDo(t, ts, "GET", "/v1/whoami", flowReq{bearer: rawKey}).want(t, 200, "whoami with the new key").body
	if flowStr(who, "key_id") != keyID || flowStr(who, "key_type") != "secret" ||
		flowStr(who, "tenant_id") != tenantID || who["livemode"] != false {
		t.Errorf("whoami = %v, want key %s, secret, account %s, test mode", who, keyID, tenantID)
	}
	if got := readCustomers(rawKey); got != 200 {
		t.Fatalf("GET /v1/customers with the new key: %d, want 200", got)
	}

	// List: the key is there with its prefix, and the list never holds the
	// secret itself.
	list := flowDo(t, ts, "GET", "/v1/api-keys", flowReq{cookie: owner}).want(t, 200, "list keys")
	listed := false
	for _, k := range flowList(list.body, "data") {
		if flowStr(k, "id") == keyID {
			listed = true
			if p := flowStr(k, "key_prefix"); p == "" || !strings.HasPrefix(rawKey, p) {
				t.Errorf("listed key_prefix %q is not the start of the raw key", p)
			}
		}
	}
	if !listed {
		t.Errorf("key %s is not in the list: %s", keyID, list.raw)
	}
	if bytes.Contains(list.raw, []byte(rawKey)) {
		t.Error("the key list contains a raw key")
	}

	// Revoke: the same key is refused at once.
	revoked := flowDo(t, ts, "DELETE", "/v1/api-keys/"+keyID, flowReq{cookie: owner}).want(t, 200, "revoke")
	if flowStr(revoked.body, "revoked_at") == "" {
		t.Errorf("revoke response has no revoked_at: %s", revoked.raw)
	}
	if got := readCustomers(rawKey); got != 401 {
		t.Errorf("GET /v1/customers with the revoked key: %d, want 401", got)
	}
	flowDo(t, ts, "GET", "/v1/whoami", flowReq{bearer: rawKey}).want(t, 401, "whoami with the revoked key")
	// Revoking it again is a 404, and a revoked key cannot be rotated
	// (api/openapi.yaml).
	flowDo(t, ts, "DELETE", "/v1/api-keys/"+keyID, flowReq{cookie: owner}).want(t, 404, "revoke a revoked key")
	flowDo(t, ts, "POST", "/v1/api-keys/"+keyID+"/rotate", flowReq{cookie: owner}).want(t, 409, "rotate a revoked key")

	// Rotate with a grace period: the old key keeps working until then, and
	// the new key works now.
	oldID, oldRaw := createKey("Billing worker", "secret")
	asked := time.Now()
	rot := flowDo(t, ts, "POST", "/v1/api-keys/"+oldID+"/rotate", flowReq{cookie: owner, body: map[string]any{
		"expires_in_seconds": 3600,
	}}).want(t, 200, "rotate with a grace period")
	newID, newRaw := flowStr(rot.body, "new_key", "id"), flowStr(rot.body, "raw_key")
	if newID == "" || newID == oldID || !strings.HasPrefix(newRaw, "vlx_secret_test_") {
		t.Fatalf("rotation returned new key %q (raw %q), want a fresh test-mode secret key", newID, newRaw)
	}
	if flowStr(rot.body, "new_key", "name") != "Billing worker" || flowStr(rot.body, "new_key", "key_type") != "secret" {
		t.Errorf("new key %v, want the old key's name and type", flowObj(rot.body, "new_key"))
	}
	graceEnd, err := time.Parse(time.RFC3339, flowStr(rot.body, "old_key", "expires_at"))
	if err != nil {
		t.Fatalf("old key expires_at: %v (body %s)", err, rot.raw)
	}
	if d := graceEnd.Sub(asked.Add(time.Hour)); d < -time.Minute || d > time.Minute {
		t.Errorf("old key expires at %s, want about an hour after the rotation (%s)", graceEnd, asked.Add(time.Hour))
	}
	if flowStr(rot.body, "old_key", "revoked_at") != "" {
		t.Error("old key was revoked; a grace rotation must leave it working")
	}
	if got := readCustomers(oldRaw); got != 200 {
		t.Errorf("old key inside its grace period: %d, want 200", got)
	}
	if got := readCustomers(newRaw); got != 200 {
		t.Errorf("new key: %d, want 200", got)
	}
	if got := flowStr(flowDo(t, ts, "GET", "/v1/whoami", flowReq{bearer: newRaw}).body, "key_id"); got != newID {
		t.Errorf("whoami with the new key names %q, want %q", got, newID)
	}

	// Rotate with no body: the old key stops at once.
	rot2 := flowDo(t, ts, "POST", "/v1/api-keys/"+newID+"/rotate", flowReq{cookie: owner}).want(t, 200, "rotate now")
	thirdID := flowStr(rot2.body, "new_key", "id")
	if flowStr(rot2.body, "old_key", "revoked_at") == "" {
		t.Errorf("rotation with no body left the old key unrevoked: %s", rot2.raw)
	}
	if got := readCustomers(newRaw); got != 401 {
		t.Errorf("key rotated with no grace: %d, want 401", got)
	}
	if got := readCustomers(flowStr(rot2.body, "raw_key")); got != 200 {
		t.Errorf("its replacement: %d, want 200", got)
	}

	// A publishable key proves which account it belongs to and reads no
	// data (docs/api-keys.md, "Key types").
	pubID, pubRaw := createKey("Checkout page", "publishable")
	if !strings.HasPrefix(pubRaw, "vlx_pub_test_") {
		t.Errorf("publishable raw key %q: want vlx_pub_test_", pubRaw)
	}
	if got := flowStr(flowDo(t, ts, "GET", "/v1/whoami", flowReq{bearer: pubRaw}).want(t, 200, "whoami with a publishable key").body, "key_id"); got != pubID {
		t.Errorf("whoami with the publishable key names %q, want %q", got, pubID)
	}
	if got := readCustomers(pubRaw); got != 403 {
		t.Errorf("GET /v1/customers with a publishable key: %d, want 403", got)
	}
	// Only a platform key may mint a platform key.
	flowDo(t, ts, "POST", "/v1/api-keys", flowReq{cookie: owner, body: map[string]any{
		"name": "escalation", "key_type": "platform",
	}}).want(t, 403, "a session minting a platform key")

	// Every change above is in the audit log, once each. A rotation's row
	// names the new key. (The bootstrap's own key rows are left out.)
	auditLog := flowDo(t, ts, "GET", "/v1/audit-log?resource_type=api_key&limit=100", flowReq{cookie: owner}).want(t, 200, "audit log")
	got := map[string]string{}
	for _, e := range flowList(auditLog.body, "data") {
		id := flowStr(e, "resource_id")
		got[id] = strings.TrimPrefix(got[id]+","+flowStr(e, "action"), ",")
	}
	for id, want := range map[string]string{
		oldID: "create", pubID: "create", newID: "rotate", thirdID: "rotate",
	} {
		if got[id] != want {
			t.Errorf("audit rows for key %s: %q, want %q", id, got[id], want)
		}
	}
	// keyID was created, then revoked; the list is newest first.
	if got[keyID] != "revoke,create" && got[keyID] != "create,revoke" {
		t.Errorf("audit rows for key %s: %q, want one create and one revoke", keyID, got[keyID])
	}
}
