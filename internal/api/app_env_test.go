package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// TestNewServer_AppEnvComesFromTheOption: APP_ENV is read once, by
// config.Load, and cmd/hardcount hands Config.Env to NewServer. Production turns
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

// TestNewServer_MetricsTokenComesFromTheEnvironment: NewServer reads
// METRICS_TOKEN through config.ServerFromEnv and hands it to /metrics, which
// then refuses a scrape without the token and serves one with it.
func TestNewServer_MetricsTokenComesFromTheEnvironment(t *testing.T) {
	t.Setenv("METRICS_TOKEN", " s3cret-scrape ")
	srv := NewServer(&postgres.DB{}, nil, nil)
	scrape := func(authorization string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := scrape(""); code != http.StatusUnauthorized {
		t.Errorf("/metrics without the token = %d, want 401", code)
	}
	if code := scrape("Bearer s3cret-scrape"); code != http.StatusOK {
		t.Errorf("/metrics with the token = %d, want 200", code)
	}
}
