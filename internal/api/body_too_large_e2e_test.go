package api

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/integrations/litellm"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestE2E_BodyOverTheCap_Answers413WithOrWithoutIdempotencyKey sends bodies
// at and past the router's cap (limitRequestBody) over the real router and
// database. A body one byte over the cap gets the same 413
// payload_too_large whether or not it carries an Idempotency-Key, because
// the idempotency layer reads the body first when there is a key and the
// handler's decoder reads it when there is none. The 413 is not stored
// under the key, so the same key then runs a normal request. The LiteLLM
// adapter keeps its own larger cap and its own 413 batch_too_large.
func TestE2E_BodyOverTheCap_Answers413WithOrWithoutIdempotencyKey(t *testing.T) {
	db := testutil.SetupTestDB(t)
	t.Setenv("HARDCOUNT_BOOTSTRAP_TOKEN", "body-cap-bootstrap")
	srv := NewServer(db, clock.Real(), nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	const email, password = "owner@body-cap.test", "body-cap-pw-12"
	flowBootstrap(t, ts, "body-cap-bootstrap", email, password)
	owner := login(t, ts, email, password)
	newKey := func(keyType string) string {
		t.Helper()
		created := flowDo(t, ts, "POST", "/v1/api-keys", flowReq{cookie: owner, body: map[string]any{"name": keyType + " key", "key_type": keyType}}).
			want(t, 201, "create a "+keyType+" key")
		return flowStr(created.body, "raw_key")
	}
	secret, ingest := newKey("secret"), newKey("ingest")

	createCustomer := func(body []byte, idemKey string) flowResp {
		t.Helper()
		req := flowReq{bearer: secret, body: body}
		if idemKey != "" {
			req.header = map[string]string{"Idempotency-Key": idemKey}
		}
		return flowDo(t, ts, "POST", "/v1/customers", req)
	}
	const wantMsg = "The request body is larger than the 1 MB limit. Send a smaller body."
	tooLarge := func(r flowResp, what string) {
		t.Helper()
		if r.status != 413 || flowStr(r.body, "error", "type") != "invalid_request_error" ||
			r.errCode() != "payload_too_large" || r.errMessage() != wantMsg {
			t.Errorf("%s: %d %s, want 413 invalid_request_error payload_too_large %q", what, r.status, r.raw, wantMsg)
		}
	}

	over := customerBodyOfSize(t, maxRequestBodyBytes+1)
	tooLarge(createCustomer(over, ""), "one byte over the cap, no Idempotency-Key")
	tooLarge(createCustomer(over, "k-big"), "one byte over the cap, with an Idempotency-Key")

	// A body exactly at the cap is read whole. The long name then fails its
	// own check, which is not a 413.
	atCap := customerBodyOfSize(t, maxRequestBodyBytes)
	for _, key := range []string{"", "k-at-cap"} {
		if r := createCustomer(atCap, key); r.status == 413 {
			t.Errorf("a body at the cap (Idempotency-Key %q): %d %s, want it read", key, r.status, r.raw)
		}
	}

	// Nothing was stored or left pending under the key the 413 carried: a
	// normal request with it runs, and its retry replays that answer.
	normal := []byte(`{"external_id":"body-cap-normal","display_name":"Acme"}`)
	first := createCustomer(normal, "k-big").want(t, 201, "a normal body under the key the 413 carried")
	if again := createCustomer(normal, "k-big"); again.status != 201 || !bytes.Equal(again.raw, first.raw) {
		t.Errorf("retry under k-big: %d %s, want the stored 201 %s", again.status, again.raw, first.raw)
	}

	// The LiteLLM adapter reads past 1 MB up to its own cap, and answers a
	// body over that cap with its own 413. These go to the router in
	// process: a 32 MB upload over loopback can run the machine's socket
	// buffers out once the server answers before reading it all.
	spend := func(size int) *httptest.ResponseRecorder {
		t.Helper()
		body := bytes.Repeat([]byte(" "), size)
		body[0], body[size-1] = '[', ']'
		req := httptest.NewRequest("POST", "/v1/integrations/litellm/spend", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+ingest)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	if rec := spend(2 << 20); rec.Code == 413 {
		t.Errorf("a 2 MB LiteLLM body: %d %s, want it read", rec.Code, rec.Body)
	}
	if rec := spend(litellm.MaxBodyBytes + 1); rec.Code != 413 || !strings.Contains(rec.Body.String(), `"code":"batch_too_large"`) {
		t.Errorf("a LiteLLM body over its cap: %d %s, want 413 batch_too_large", rec.Code, rec.Body)
	}
}

// customerBodyOfSize is a customer create body of exactly size bytes, padded
// in display_name.
func customerBodyOfSize(t *testing.T, size int) []byte {
	t.Helper()
	const head, tail = `{"external_id":"body-cap","display_name":"`, `"}`
	pad := size - len(head) - len(tail)
	if pad < 0 {
		t.Fatalf("size %d is under the %d bytes of the body's frame", size, len(head)+len(tail))
	}
	return []byte(head + string(bytes.Repeat([]byte("a"), pad)) + tail)
}
