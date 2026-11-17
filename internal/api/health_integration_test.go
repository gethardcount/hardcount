package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/testutil"
)

// Through the server on a real database: liveness answers 200 {"status":"ok"}
// in JSON, before and during a drain. Readiness answers 200 ok until
// SetDraining, then 503 draining, so a load balancer stops sending traffic
// before the listener closes while an orchestrator keeps the replica alive.
func TestHealth_ReadinessFailsFirstWhenDrainingAndLivenessStays200(t *testing.T) {
	db := testutil.SetupTestDB(t)
	srv := NewServer(db, nil, nil)
	t.Cleanup(func() { draining.Store(false) })

	get := func(path string) (int, string, map[string]any) {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, rec.Header().Get("Content-Type"), body
	}

	if code, ct, body := get("/health"); code != http.StatusOK || !strings.Contains(ct, "application/json") || body["status"] != "ok" {
		t.Fatalf("/health = %d %q %v, want 200 application/json {status: ok}", code, ct, body)
	}
	if code, _, body := get("/health/ready"); code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("before drain: /health/ready = %d %v, want 200 ok", code, body)
	}

	SetDraining()

	code, _, body := get("/health/ready")
	if code != http.StatusServiceUnavailable || body["status"] != "draining" {
		t.Fatalf("draining: /health/ready = %d %v, want 503 draining", code, body)
	}
	if checks, _ := body["checks"].(map[string]any); checks["api"] != "draining" {
		t.Fatalf("draining: checks.api = %v, want draining", checks["api"])
	}
	if code, _, _ := get("/health"); code != http.StatusOK {
		t.Fatalf("draining: /health = %d, want 200 (liveness must not fail during drain)", code)
	}
}
