package api

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/testutil"
)

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
