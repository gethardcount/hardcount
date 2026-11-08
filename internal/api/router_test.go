package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// newTestServer builds the production Server on a zero DB. NewServer runs
// engine.MustValidate after it wires the engine, so every test that calls
// this also proves that no engine collaborator is missing.
func newTestServer() *Server {
	// Create server with nil DB — handlers that don't touch DB will work,
	// auth middleware will reject (no valid keys), which is what we want to test.
	db := &postgres.DB{}
	return NewServer(db, nil, nil)
}

func TestHealthEndpoint(t *testing.T) {
	srv := newTestServer()

	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type: got %q, want application/json", ct)
	}

	var body map[string]string
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("body: got %v", body)
	}
}

func TestAuthRequired_AllProtectedRoutes(t *testing.T) {
	srv := newTestServer()

	routes := []struct {
		method string
		path   string
	}{
		{"GET", "/v1/customers"},
		{"POST", "/v1/customers"},
		{"GET", "/v1/meters"},
		{"GET", "/v1/plans"},
		{"GET", "/v1/plans/pln_123/prices"},
		{"GET", "/v1/subscriptions"},
		{"GET", "/v1/usage-events"},
		{"GET", "/v1/invoices"},
		{"GET", "/v1/api-keys"},
		{"GET", "/v1/dunning/policy"},
		{"GET", "/v1/credits/balance/cus_123"},
	}

	for _, r := range routes {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			req := httptest.NewRequest(r.method, r.path, nil)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s: got %d, want 401", r.method, r.path, rec.Code)
			}
			var body map[string]any
			_ = json.NewDecoder(rec.Body).Decode(&body)
			errObj, _ := body["error"].(map[string]any)
			if errObj == nil || errObj["type"] != "authentication_error" {
				t.Errorf("%s %s error type: got %v, want authentication_error", r.method, r.path, body["error"])
			}
			// Rate limiting runs after auth, so a 401 carries no rate-limit
			// headers.
			if got := rec.Header().Get("X-RateLimit-Limit"); got != "" {
				t.Errorf("%s %s: X-RateLimit-Limit = %q on an unauthenticated request, want none", r.method, r.path, got)
			}
		})
	}
}
