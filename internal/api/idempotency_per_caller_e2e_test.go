package api

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestE2E_IdempotencyKey_AnswersOnlyTheCallerThatMadeTheRequest sends one
// Idempotency-Key from several callers of one account and mode, over the
// real router and database. The caller is the API key, or the dashboard
// user, that made the request. Every caller that holds the route's
// permission reaches the idempotency layer, so a stored answer must go back
// only to the caller that made the request: a secret key's retry replays it,
// while another secret key, a dashboard user, or the same key with another
// query string gets the key-reuse 422 and none of the stored body. A
// dashboard user who signs in again is still the same caller, so the retry
// replays. A request refused by its route's permission, or one that matched
// no route, holds no key: the next caller's request with it runs.
func TestE2E_IdempotencyKey_AnswersOnlyTheCallerThatMadeTheRequest(t *testing.T) {
	db := testutil.SetupTestDB(t)
	t.Setenv("HARDCOUNT_BOOTSTRAP_TOKEN", "idem-caller-bootstrap")
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	const email, password = "owner@idem-caller.test", "idem-caller-pw-12"
	flowBootstrap(t, ts, "idem-caller-bootstrap", email, password)
	owner := login(t, ts, email, password)
	newKey := func(keyType string) string {
		t.Helper()
		created := flowDo(t, ts, "POST", "/v1/api-keys", flowReq{cookie: owner, body: map[string]any{"name": keyType + " key", "key_type": keyType}}).
			want(t, 201, "create a "+keyType+" key")
		return flowStr(created.body, "raw_key")
	}
	secret, otherSecret, ingest := newKey("secret"), newKey("secret"), newKey("ingest")

	reuse := func(r flowResp, who, firstID string) {
		t.Helper()
		const msg = "This Idempotency-Key was first used by another caller or with a different request. Use a new key for a new request."
		if r.status != 422 || r.errCode() != "idempotency_error" || r.errMessage() != msg {
			t.Errorf("%s: %d %s, want 422 idempotency_error %q", who, r.status, r.raw, msg)
		}
		if strings.Contains(string(r.raw), firstID) {
			t.Errorf("%s: the answer %s carries the stored answer's customer %s", who, r.raw, firstID)
		}
	}
	replay := func(r flowResp, who string, first flowResp) {
		t.Helper()
		if r.status != 201 || string(r.raw) != string(first.raw) {
			t.Errorf("%s: %d %s, want the stored 201 %s", who, r.status, r.raw, first.raw)
		}
	}
	create := func(req flowReq, path, idemKey string) flowResp {
		t.Helper()
		req.header = map[string]string{"Idempotency-Key": idemKey}
		req.body = map[string]any{"external_id": "idem-cred-" + idemKey, "display_name": "Acme"}
		return flowDo(t, ts, "POST", path, req)
	}

	// A secret key creates a customer; another caller of the same
	// account and mode sends the same method, path, body and key.
	first := create(flowReq{bearer: secret}, "/v1/customers", "k-cred").want(t, 201, "secret key creates a customer")
	firstID := flowStr(first.body, "id")
	reuse(create(flowReq{bearer: otherSecret}, "/v1/customers", "k-cred"), "another secret key reusing the first's key", firstID)
	reuse(create(flowReq{cookie: owner}, "/v1/customers", "k-cred"), "dashboard session reusing the secret key's key", firstID)
	replay(create(flowReq{bearer: secret}, "/v1/customers", "k-cred"), "secret key retrying", first)

	// The query string is part of the request.
	firstQ := create(flowReq{bearer: secret}, "/v1/customers?source=a", "k-query").want(t, 201, "secret key creates with a query string")
	replay(create(flowReq{bearer: secret}, "/v1/customers?source=a", "k-query"), "same query string", firstQ)
	reuse(create(flowReq{bearer: secret}, "/v1/customers?source=b", "k-query"), "another query string", flowStr(firstQ.body, "id"))
	reuse(create(flowReq{bearer: secret}, "/v1/customers", "k-query"), "no query string", flowStr(firstQ.body, "id"))

	// A dashboard user who signs in again is the same caller.
	firstS := create(flowReq{cookie: owner}, "/v1/customers", "k-session").want(t, 201, "dashboard session creates a customer")
	again := login(t, ts, email, password)
	if again.Value == owner.Value {
		t.Fatal("the second sign-in reused the first session; the test needs two sessions")
	}
	replay(create(flowReq{cookie: again}, "/v1/customers", "k-session"), "same user in a new session", firstS)

	rows := func(key string) int {
		t.Helper()
		tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		var n int
		if err := tx.QueryRowContext(t.Context(), `SELECT count(*) FROM idempotency_keys WHERE key = $1`, key).Scan(&n); err != nil {
			t.Fatalf("count idempotency rows: %v", err)
		}
		return n
	}

	// The route's permission (customer:write) refuses the ingest key before
	// Idempotency, so nothing is reserved: the secret key's request with the
	// key runs, and its answer is stored.
	if r := create(flowReq{bearer: ingest}, "/v1/customers", "k-refused"); r.status != 403 {
		t.Fatalf("ingest key creating a customer: %d %s, want 403", r.status, r.raw)
	}
	if n := rows("k-refused"); n != 0 {
		t.Errorf("after the 403: %d idempotency rows, want none", n)
	}
	firstR := create(flowReq{bearer: secret}, "/v1/customers", "k-refused").want(t, 201, "secret key after the refused request")
	replay(create(flowReq{bearer: secret}, "/v1/customers", "k-refused"), "secret key retrying after the refusal", firstR)

	// The customer-usage mount routes no POST: the ingest key's request
	// answers 405 and reserves nothing, so the secret key's request with the
	// key runs.
	unrouted := flowDo(t, ts, "POST", "/v1/customers/x/usage", flowReq{bearer: ingest,
		header: map[string]string{"Idempotency-Key": "k-unrouted"}, body: map[string]any{}})
	if unrouted.status != 405 {
		t.Fatalf("ingest key posting under the usage mount: %d %s, want 405", unrouted.status, unrouted.raw)
	}
	if n := rows("k-unrouted"); n != 0 {
		t.Errorf("after the 405: %d idempotency rows, want none", n)
	}
	create(flowReq{bearer: secret}, "/v1/customers", "k-unrouted").want(t, 201, "secret key after the unrouted request")

	// A typo under a mount answers 404 and reserves nothing either.
	typo := flowDo(t, ts, "POST", "/v1/invoices/x/typo", flowReq{bearer: secret,
		header: map[string]string{"Idempotency-Key": "k-typo"}, body: map[string]any{}})
	if typo.status != 404 {
		t.Fatalf("a typo under the invoices mount: %d %s, want 404", typo.status, typo.raw)
	}
	if n := rows("k-typo"); n != 0 {
		t.Errorf("after the 404: %d idempotency rows, want none", n)
	}
	create(flowReq{bearer: secret}, "/v1/customers", "k-typo").want(t, 201, "secret key after the 404")
}
