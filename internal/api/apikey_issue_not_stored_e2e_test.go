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

// The other requests whose answer carries a secret shown only once: creating
// a webhook endpoint, rotating its signing secret, and rotating a customer's
// cost dashboard token. Each sent twice with the same Idempotency-Key runs
// twice, and no answer is kept. (Creating a card setup session needs a
// connected Stripe account; its route is pinned by TestAnswersWithShowOnceSecret
// and TestOpenAPISpec_ShowOnceSecretsAreNeverKept.)
func TestE2E_ShowOnceSecret_AnswerIsNotKeptForReplay(t *testing.T) {
	db := testutil.SetupTestDB(t)
	t.Setenv("HARDCOUNT_BOOTSTRAP_TOKEN", "show-once-bootstrap")
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	const email, password = "owner@show-once.test", "show-once-pw-1234"
	flowBootstrap(t, ts, "show-once-bootstrap", email, password)
	owner := login(t, ts, email, password)
	twice := func(path, idem string, body any, status int) (first, again map[string]any) {
		t.Helper()
		req := flowReq{cookie: owner, header: map[string]string{"Idempotency-Key": idem}, body: body}
		return flowDo(t, ts, "POST", path, req).want(t, status, path).body,
			flowDo(t, ts, "POST", path, req).want(t, status, path+" again with the same Idempotency-Key").body
	}

	ep, epAgain := twice("/v1/webhook-endpoints/endpoints", "show-once-endpoint", map[string]any{"url": "https://example.com/show-once"}, 201)
	if flowStr(ep, "endpoint", "id") == flowStr(epAgain, "endpoint", "id") || flowStr(ep, "secret") == flowStr(epAgain, "secret") {
		t.Errorf("webhook endpoint create answered endpoint %s again: replayed, want a second endpoint", flowStr(ep, "endpoint", "id"))
	}
	rot, rotAgain := twice("/v1/webhook-endpoints/endpoints/"+flowStr(epAgain, "endpoint", "id")+"/rotate-secret", "show-once-rotate", nil, 200)
	if flowStr(rot, "secret") == flowStr(rotAgain, "secret") {
		t.Error("signing secret rotation answered the same secret again: replayed, want a new one")
	}
	cust := flowStr(flowDo(t, ts, "POST", "/v1/customers", flowReq{cookie: owner,
		body: map[string]any{"external_id": "show-once-cus", "display_name": "Show Once"}}).want(t, 201, "create customer").body, "id")
	dash, dashAgain := twice("/v1/customers/"+cust+"/rotate-cost-dashboard-token", "show-once-dashboard", nil, 200)
	if flowStr(dash, "token") == flowStr(dashAgain, "token") {
		t.Error("cost dashboard token rotation answered the same token again: replayed, want a new one")
	}

	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var kept int
	if err := tx.QueryRow(`SELECT count(*) FROM idempotency_keys WHERE key LIKE 'show-once-%'`).Scan(&kept); err != nil {
		t.Fatalf("read idempotency_keys: %v", err)
	}
	if kept != 0 {
		t.Errorf("idempotency_keys holds %d answers for these requests, want none", kept)
	}
}
