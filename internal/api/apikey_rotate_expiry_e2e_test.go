package api

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

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

	// A key with no expiry gets the grace as its expiry, as before.
	open := create("No expiry", nil)
	before := time.Now().UTC()
	rotated = flowDo(t, ts, "POST", "/v1/api-keys/"+open+"/rotate", flowReq{cookie: owner,
		body: map[string]any{"expires_in_seconds": 3600}}).want(t, 200, "rotate with 1 hour of grace").body
	if got, err := time.Parse(time.RFC3339Nano, flowStr(rotated, "old_key", "expires_at")); err != nil ||
		got.Before(before.Add(time.Hour).Add(-time.Second)) || got.After(time.Now().UTC().Add(time.Hour)) {
		t.Errorf("old key expires_at = %q, want an hour from the rotation", flowStr(rotated, "old_key", "expires_at"))
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
