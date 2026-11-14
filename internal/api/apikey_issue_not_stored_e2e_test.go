package api

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// A request that issues an API key, a create or a rotate, returns the key's
// secret once and stores it nowhere: an Idempotency-Key on it is ignored, so
// the answer is not kept for a replay. Sending the same request again runs it
// again.
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
