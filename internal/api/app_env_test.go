package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// TestNewServer_AppEnvComesFromTheOption: APP_ENV is read once, by
// config.Load, and cmd/velox hands Config.Env to NewServer. Production turns
// on HSTS and closes /metrics when METRICS_TOKEN is unset. With no
// WithAppEnv the server runs as local, like an unset APP_ENV, even when the
// process environment says production: nothing below config reads APP_ENV.
func TestNewServer_AppEnvComesFromTheOption(t *testing.T) {
	t.Setenv("METRICS_TOKEN", "")
	t.Setenv("APP_ENV", "production") // must be ignored: only the option counts

	get := func(srv *Server, path string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	prod := NewServer(&postgres.DB{}, nil, nil, WithAppEnv("production"))
	if get(prod, "/health").Header().Get("Strict-Transport-Security") == "" {
		t.Error("production: /health has no HSTS header")
	}
	if code := get(prod, "/metrics").Code; code != http.StatusUnauthorized {
		t.Errorf("production without METRICS_TOKEN: /metrics = %d, want 401", code)
	}

	local := NewServer(&postgres.DB{}, nil, nil)
	if hsts := get(local, "/health").Header().Get("Strict-Transport-Security"); hsts != "" {
		t.Errorf("no WithAppEnv: /health sent HSTS %q; want none, as in local", hsts)
	}
}
