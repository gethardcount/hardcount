package api

// End-to-end tests of the API keys flow over the real router and database: the
// lifecycle (create, use, list, revoke, rotate), what a key request stores,
// rotation expiry and the key routes wording.

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestE2E_APIKeyLifecycle walks the API keys page over the real router and
// database: create a key from a signed-in session, use it, find it in the
// list, revoke it and see the same key refused, then rotate the two ways
// docs/api-keys.md describes. A grace period keeps the old key working;
// no body retires it at once. The list says each key's status.
func TestE2E_APIKeyLifecycle(t *testing.T) {
	db := testutil.SetupTestDB(t)
	t.Setenv("HARDCOUNT_BOOTSTRAP_TOKEN", "apikey-flow-bootstrap")
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
	if !strings.HasPrefix(rawKey, "hc_secret_test_") {
		t.Fatalf("raw key %q: want an hc_secret_test_ key (the session is in test mode)", rawKey)
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

	// List: the key is there, active, with its prefix and, since it was just
	// used, its last use. The list never holds the secret itself.
	list := flowDo(t, ts, "GET", "/v1/api-keys", flowReq{cookie: owner}).want(t, 200, "list keys")
	listed := false
	for _, k := range flowList(list.body, "data") {
		if flowStr(k, "id") == keyID {
			listed = true
			if p := flowStr(k, "key_prefix"); p == "" || !strings.HasPrefix(rawKey, p) {
				t.Errorf("listed key_prefix %q is not the start of the raw key", p)
			}
			if flowStr(k, "last_used_at") == "" {
				t.Errorf("the key was used and its last_used_at is empty: %v", k)
			}
			if got := flowStr(k, "status"); got != "active" {
				t.Errorf("listed status %q, want active", got)
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
	if newID == "" || newID == oldID || !strings.HasPrefix(newRaw, "hc_secret_test_") {
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

	// List again: the revoked key says revoked, and the rotated key is still
	// active, with the end of its grace period as its expires_at.
	listedNow := map[string]map[string]any{}
	for _, k := range flowList(flowDo(t, ts, "GET", "/v1/api-keys", flowReq{cookie: owner}).want(t, 200, "list after the rotation").body, "data") {
		listedNow[flowStr(k, "id")] = k
	}
	if got := flowStr(listedNow[keyID], "status"); got != "revoked" {
		t.Errorf("the revoked key's listed status is %q, want revoked", got)
	}
	if k := listedNow[oldID]; flowStr(k, "status") != "active" || flowStr(k, "expires_at") != flowStr(rot.body, "old_key", "expires_at") {
		t.Errorf("the rotated key is listed with status %q, expires_at %q; want active until the grace ends (%s)",
			flowStr(k, "status"), flowStr(k, "expires_at"), flowStr(rot.body, "old_key", "expires_at"))
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

	// There is no platform key type (ADR-133): a create of one is refused,
	// and /v1/tenants is not served.
	flowDo(t, ts, "POST", "/v1/api-keys", flowReq{cookie: owner, body: map[string]any{
		"name": "escalation", "key_type": "platform",
	}}).want(t, 422, "create a key of the removed platform type")
	for _, m := range []string{"GET", "POST"} {
		if got := flowDo(t, ts, m, "/v1/tenants", flowReq{cookie: owner, body: map[string]any{"name": "Other"}}).status; got != 404 {
			t.Errorf("%s /v1/tenants: %d, want 404 (the route is gone)", m, got)
		}
	}

	// Every change above is in the audit log, once each. A rotation's row
	// names the new key. (The bootstrap's own key rows are left out.)
	auditLog := flowDo(t, ts, "GET", "/v1/audit-log?resource_type=api_key&limit=100", flowReq{cookie: owner}).want(t, 200, "audit log")
	got := map[string]string{}
	for _, e := range flowList(auditLog.body, "data") {
		id := flowStr(e, "resource_id")
		got[id] = strings.TrimPrefix(got[id]+","+flowStr(e, "action"), ",")
	}
	for id, want := range map[string]string{
		oldID: "create", newID: "rotate", thirdID: "rotate",
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

// A request that issues an API key, a create or a rotate, returns the key's
// secret once and stores it nowhere: an Idempotency-Key on it is ignored, so
// the answer is not kept for a replay. Sending the same request again runs it
// again. The other requests that answer with a secret shown once skip the
// idempotency layer by the same list (TestAnswersWithShowOnceSecret).
func TestE2E_APIKeyIssue_SecretIsNotStoredForReplay(t *testing.T) {
	db := testutil.SetupTestDB(t)
	t.Setenv("HARDCOUNT_BOOTSTRAP_TOKEN", "apikey-issue-bootstrap")
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	const email, password = "owner@apikey-issue.test", "apikey-issue-pw-12"
	flowBootstrap(t, ts, "apikey-issue-bootstrap", email, password)
	owner := login(t, ts, email, password)

	create := flowReq{cookie: owner, header: map[string]string{"Idempotency-Key": "apikey-issue-create"},
		body: map[string]any{"name": "CI server", "key_type": "secret"}}
	first := flowDo(t, ts, "POST", "/v1/api-keys", create).want(t, 201, "create")
	again := flowDo(t, ts, "POST", "/v1/api-keys", create).want(t, 201, "create again with the same Idempotency-Key")
	if flowStr(again.body, "raw_key") == flowStr(first.body, "raw_key") || flowStr(again.body, "key", "id") == flowStr(first.body, "key", "id") {
		t.Errorf("the second create answered key %s again: the secret was replayed, want a new key", flowStr(first.body, "key", "id"))
	}

	// Rotate with no body retires the old key at once, so the same rotate sent
	// again finds it revoked.
	rotatePath := "/v1/api-keys/" + flowStr(first.body, "key", "id") + "/rotate"
	rotate := flowReq{cookie: owner, header: map[string]string{"Idempotency-Key": "apikey-issue-rotate"}}
	flowDo(t, ts, "POST", rotatePath, rotate).want(t, 200, "rotate")
	flowDo(t, ts, "POST", rotatePath, rotate).want(t, 409, "rotate again with the same Idempotency-Key: the key is revoked now, not a replayed answer")

	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var stored, withSecret int
	if err := tx.QueryRow(`SELECT count(*) FILTER (WHERE http_path LIKE '/v1/api-keys%'),
		count(*) FILTER (WHERE convert_from(response_body, 'UTF8') LIKE '%hc_secret_%')
		FROM idempotency_keys`).Scan(&stored, &withSecret); err != nil {
		t.Fatalf("read idempotency_keys: %v", err)
	}
	if stored != 0 || withSecret != 0 {
		t.Errorf("idempotency_keys: %d rows for /v1/api-keys, %d holding a secret key; want none", stored, withSecret)
	}
}

// A rotation's grace period never keeps the old key working past the expiry
// it already had, and a key that has expired cannot be rotated.
func TestE2E_APIKeyRotate_NeverLengthensTheOldKeysLife(t *testing.T) {
	db := testutil.SetupTestDB(t)
	t.Setenv("HARDCOUNT_BOOTSTRAP_TOKEN", "apikey-rotate-bootstrap")
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	const email, password = "owner@apikey-rotate.test", "apikey-rotate-pw-12"
	flowBootstrap(t, ts, "apikey-rotate-bootstrap", email, password)
	owner := login(t, ts, email, password)
	create := func(name string, expiresAt *time.Time) string {
		t.Helper()
		body := map[string]any{"name": name, "key_type": "secret"}
		if expiresAt != nil {
			body["expires_at"] = expiresAt.Format(time.RFC3339)
		}
		return flowStr(flowDo(t, ts, "POST", "/v1/api-keys", flowReq{cookie: owner, body: body}).want(t, 201, "create "+name).body, "key", "id")
	}

	// Due to expire in an hour, rotated with the longest grace (7 days): the
	// old key still stops in an hour.
	due := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	soon := create("Expiring soon", &due)
	rotated := flowDo(t, ts, "POST", "/v1/api-keys/"+soon+"/rotate", flowReq{cookie: owner,
		body: map[string]any{"expires_in_seconds": 604800}}).want(t, 200, "rotate with 7 days of grace").body
	if got, err := time.Parse(time.RFC3339Nano, flowStr(rotated, "old_key", "expires_at")); err != nil || !got.Equal(due) {
		t.Errorf("old key expires_at = %q, want its own %s: a rotation must not lengthen it", flowStr(rotated, "old_key", "expires_at"), due.Format(time.RFC3339))
	}

	// An expired key. No request can make one, since a create refuses a past
	// expiry, so the test moves a key's expiry into the past directly.
	gone := create("Expired", &due)
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(`UPDATE api_keys SET expires_at = now() - interval '1 minute' WHERE id = $1`, gone); err != nil {
		t.Fatalf("age the key: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	countKeys := func() int {
		t.Helper()
		return len(flowDo(t, ts, "GET", "/v1/api-keys", flowReq{cookie: owner}).want(t, 200, "list").body["data"].([]any))
	}
	keysBefore := countKeys()
	res := flowDo(t, ts, "POST", "/v1/api-keys/"+gone+"/rotate", flowReq{cookie: owner}).want(t, 409, "rotate an expired key")
	if code := flowStr(res.body, "error", "code"); code != "invalid_state" {
		t.Errorf("error code = %q, want invalid_state", code)
	}
	if n := countKeys(); n != keysBefore {
		t.Errorf("keys = %d after the refused rotation, want %d: it must make no key", n, keysBefore)
	}
}

// The key routes answer one 404 wording, and a new key's name is 1 to 100
// characters, counted as characters, not bytes.
func TestE2E_APIKeyRoutes_OneNotFoundWordingAndOneNameRule(t *testing.T) {
	db := testutil.SetupTestDB(t)
	t.Setenv("HARDCOUNT_BOOTSTRAP_TOKEN", "key-wording-bootstrap")
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	const email, password = "owner@key-wording.test", "key-wording-pw-123"
	flowBootstrap(t, ts, "key-wording-bootstrap", email, password)
	owner := login(t, ts, email, password)

	for _, c := range []struct{ method, path string }{
		{"DELETE", "/v1/api-keys/hc_key_unknown"},
		{"POST", "/v1/api-keys/hc_key_unknown/rotate"},
	} {
		body := flowDo(t, ts, c.method, c.path, flowReq{cookie: owner}).want(t, 404, c.method+" an unknown key").body
		if msg := flowStr(body, "error", "message"); msg != "api key not found" {
			t.Errorf("%s %s: message %q, want %q", c.method, c.path, msg, "api key not found")
		}
	}

	create := func(name string) flowResp {
		return flowDo(t, ts, "POST", "/v1/api-keys", flowReq{cookie: owner, body: map[string]any{"name": name, "key_type": "secret"}})
	}
	tooLong := create(strings.Repeat("n", 101)).want(t, 422, "create a key with a 101-character name").body
	if p := flowStr(tooLong, "error", "param"); p != "name" {
		t.Errorf("101-character name: param %q, want name", p)
	}
	create(strings.Repeat("n", 100)).want(t, 201, "create a key with a 100-character name")
	create(strings.Repeat("é", 100)).want(t, 201, "create a key with a 100-character name of two-byte characters")
}
