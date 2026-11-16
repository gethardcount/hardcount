package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// The access log through the whole server: each request's line names the API
// key or dashboard session it was made with and the address it came from,
// and says "anonymous" when no caller was accepted.
func TestAccessLog_ThroughTheServer(t *testing.T) {
	db := testutil.SetupTestDB(t)
	t.Setenv("HARDCOUNT_BOOTSTRAP_TOKEN", "access-log-bootstrap")
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	t.Cleanup(ts.Close)

	const email, password = "owner@access-log.test", "access-log-pw-12"
	tenantID := flowStr(flowBootstrap(t, ts, "access-log-bootstrap", email, password), "tenant", "id")
	owner := login(t, ts, email, password)
	userID := flowStr(flowDo(t, ts, "GET", "/v1/whoami", flowReq{cookie: owner}).want(t, 200, "whoami").body, "user_id")
	created := flowDo(t, ts, "POST", "/v1/api-keys", flowReq{cookie: owner,
		body: map[string]any{"name": "Access log", "key_type": "secret"}}).want(t, 201, "create key").body
	keyID, rawKey := flowStr(created, "key", "id"), flowStr(created, "raw_key")
	ingest := flowDo(t, ts, "POST", "/v1/api-keys", flowReq{cookie: owner,
		body: map[string]any{"name": "Access log ingest", "key_type": "ingest"}}).want(t, 201, "create ingest key").body
	ingestKeyID, ingestKey := flowStr(ingest, "key", "id"), flowStr(ingest, "raw_key")
	if tenantID == "" || userID == "" || keyID == "" || ingestKeyID == "" {
		t.Fatalf("setup: tenant %q, user %q, key %q, ingest key %q", tenantID, userID, keyID, ingestKeyID)
	}

	lines := captureAccessLog(t)
	// Every request below comes from this machine with no proxy in front
	// (TRUST_PROXY unset), so its address is 127.0.0.1.
	//
	// call makes one request with the credential setAuth adds and returns
	// that request's line, found by the request id in its answer.
	call := func(t *testing.T, method, path, body string, wantStatus int, setAuth func(*http.Request)) map[string]string {
		t.Helper()
		req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		setAuth(req)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		_ = res.Body.Close()
		if res.StatusCode != wantStatus {
			t.Fatalf("%s %s: status %d, want %d", method, path, res.StatusCode, wantStatus)
		}
		id := res.Header.Get("Hardcount-Request-Id")
		if id == "" {
			t.Fatal("the answer has no Hardcount-Request-Id")
		}
		return lines.byRequestID(t, id)
	}
	bearer := func(key string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+key) }
	}
	cookie := func(r *http.Request) { r.AddCookie(owner) }
	wantLine := func(t *testing.T, line map[string]string, want map[string]string) {
		t.Helper()
		for key, w := range want {
			if got := line[key]; got != w {
				t.Errorf("access log %s = %q, want %q", key, got, w)
			}
		}
	}

	wantAnonymous := func(t *testing.T, line map[string]string, status string) {
		t.Helper()
		wantLine(t, line, map[string]string{"actor_type": "anonymous", "ip": "127.0.0.1", "status": status})
		for _, key := range []string{"actor_id", "tenant_id"} {
			if got, ok := line[key]; ok {
				t.Errorf("%s = %q, want the field left out: no caller was accepted", key, got)
			}
		}
	}
	asKey := map[string]string{"actor_type": "api_key", "actor_id": keyID, "tenant_id": tenantID, "ip": "127.0.0.1"}
	asUser := map[string]string{"actor_type": "user", "actor_id": userID, "tenant_id": tenantID, "ip": "127.0.0.1"}

	t.Run("an API key", func(t *testing.T) {
		wantLine(t, call(t, "GET", "/v1/customers", "", 200, bearer(rawKey)), asKey)
	})

	t.Run("a dashboard session", func(t *testing.T) {
		wantLine(t, call(t, "GET", "/v1/customers", "", 200, cookie), asUser)
	})

	// A caller that was accepted and then refused is still named.
	t.Run("an API key refused by its permissions", func(t *testing.T) {
		line := call(t, "GET", "/v1/customers", "", 403, bearer(ingestKey))
		wantLine(t, line, map[string]string{"actor_type": "api_key", "actor_id": ingestKeyID, "tenant_id": tenantID, "status": "403"})
	})

	t.Run("no credentials", func(t *testing.T) {
		wantAnonymous(t, call(t, "GET", "/v1/customers", "", 401, func(*http.Request) {}), "401")
	})

	t.Run("a key that is not valid", func(t *testing.T) {
		wantAnonymous(t, call(t, "GET", "/v1/customers", "", 401, bearer("hc_secret_test_"+strings.Repeat("0", 64))), "401")
	})

	// The mode switch and sign-out read the session cookie themselves, with
	// no authentication middleware in front.
	t.Run("the mode switch", func(t *testing.T) {
		wantLine(t, call(t, "POST", "/v1/auth/mode", `{"livemode":false}`, 200, cookie), asUser)
	})

	t.Run("sign-out", func(t *testing.T) {
		wantLine(t, call(t, "POST", "/v1/auth/logout", "", 204, cookie), asUser)
		// The session is gone: the same cookie now names nobody.
		wantAnonymous(t, call(t, "GET", "/v1/customers", "", 401, cookie), "401")
	})
}
