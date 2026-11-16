package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// The access log through the whole server: a request made with an API key,
// one made with a dashboard session and one with no credentials each get a
// line that names the caller the server accepted and the address it came
// from.
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
	if tenantID == "" || userID == "" || keyID == "" {
		t.Fatalf("setup: tenant %q, user %q, key %q", tenantID, userID, keyID)
	}

	lines := captureAccessLog(t)
	// list calls GET /v1/customers with the credential setAuth adds and
	// returns that request's line, found by the request id in its answer.
	list := func(t *testing.T, wantStatus int, setAuth func(*http.Request)) map[string]string {
		t.Helper()
		req, err := http.NewRequest("GET", ts.URL+"/v1/customers", nil)
		if err != nil {
			t.Fatal(err)
		}
		setAuth(req)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /v1/customers: %v", err)
		}
		_ = res.Body.Close()
		if res.StatusCode != wantStatus {
			t.Fatalf("GET /v1/customers: status %d, want %d", res.StatusCode, wantStatus)
		}
		id := res.Header.Get("Hardcount-Request-Id")
		if id == "" {
			t.Fatal("the answer has no Hardcount-Request-Id")
		}
		return lines.byRequestID(t, id)
	}
	wantLine := func(t *testing.T, line map[string]string, want map[string]string) {
		t.Helper()
		for key, w := range want {
			if got := line[key]; got != w {
				t.Errorf("access log %s = %q, want %q", key, got, w)
			}
		}
	}

	t.Run("an API key", func(t *testing.T) {
		line := list(t, 200, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+rawKey) })
		wantLine(t, line, map[string]string{"actor_type": "api_key", "actor_id": keyID, "tenant_id": tenantID, "ip": "127.0.0.1", "status": "200"})
	})

	t.Run("a dashboard session", func(t *testing.T) {
		line := list(t, 200, func(r *http.Request) { r.AddCookie(owner) })
		wantLine(t, line, map[string]string{"actor_type": "user", "actor_id": userID, "tenant_id": tenantID, "ip": "127.0.0.1", "status": "200"})
	})

	t.Run("no credentials", func(t *testing.T) {
		line := list(t, 401, func(*http.Request) {})
		wantLine(t, line, map[string]string{"actor_type": "anonymous", "ip": "127.0.0.1", "status": "401"})
		for _, key := range []string{"actor_id", "tenant_id"} {
			if got, ok := line[key]; ok {
				t.Errorf("%s = %q, want the field left out: nobody signed in", key, got)
			}
		}
	})
}
