package session

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// The API key half of MiddlewareOrAPIKey, on real Postgres: what a request
// with no credentials, a key that is not valid, a valid key in either header,
// and a key checked while the database is down each get.

// keyRequest is a request with no cookie, carrying key in header (empty: none).
func keyRequest(header, key string) *http.Request {
	req := httptest.NewRequest("GET", "/", nil)
	if key != "" {
		if header == "Authorization" {
			req.Header.Set(header, "Bearer "+key)
		} else {
			req.Header.Set(header, key)
		}
	}
	return req
}

func TestMiddlewareOrAPIKey_APIKey(t *testing.T) {
	db := testutil.SetupTestDB(t)
	keys := auth.NewService(db, audit.NewLogger(db))
	tenantID := testutil.CreateTestTenant(t, db, "Session API Key")
	created, err := keys.CreateKey(postgres.WithLivemode(t.Context(), false), tenantID, auth.CreateKeyInput{Name: "CI", KeyType: auth.KeyTypeSecret})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	sessions := NewService(&fakeSessionStore{sessions: map[string]Session{}})

	serve := func(svc *auth.Service, req *http.Request) (*httptest.ResponseRecorder, *http.Request) {
		t.Helper()
		var seen *http.Request
		rec := httptest.NewRecorder()
		MiddlewareOrAPIKey(sessions, svc)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = r
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(rec, req)
		return rec, seen
	}

	t.Run("no credentials", func(t *testing.T) {
		rec, seen := serve(keys, keyRequest("", ""))
		if rec.Code != http.StatusUnauthorized || seen != nil || !strings.Contains(rec.Body.String(), "missing credentials") {
			t.Errorf("got %d %s (reached=%v), want 401 missing credentials", rec.Code, rec.Body.String(), seen != nil)
		}
	})

	t.Run("a key that is not valid", func(t *testing.T) {
		rec, seen := serve(keys, keyRequest("Authorization", "hc_secret_test_"+strings.Repeat("0", 64)))
		if rec.Code != http.StatusUnauthorized || seen != nil || !strings.Contains(rec.Body.String(), "invalid or expired API key") {
			t.Errorf("got %d %s (reached=%v), want 401 with the generic message", rec.Code, rec.Body.String(), seen != nil)
		}
	})

	for _, header := range []string{"Authorization", "X-API-Key"} {
		t.Run("a valid key in "+header, func(t *testing.T) {
			rec, seen := serve(keys, keyRequest(header, created.RawKey))
			if rec.Code != http.StatusOK || seen == nil {
				t.Fatalf("got %d %s, want 200", rec.Code, rec.Body.String())
			}
			ctx := seen.Context()
			if auth.TenantID(ctx) != tenantID || auth.KeyID(ctx) != created.Key.ID || auth.GetKeyType(ctx) != auth.KeyTypeSecret {
				t.Errorf("principal = tenant %q, key %q, type %q; want %s, %s, secret", auth.TenantID(ctx), auth.KeyID(ctx), auth.GetKeyType(ctx), tenantID, created.Key.ID)
			}
			if live, err := postgres.Livemode(ctx); err != nil || live {
				t.Errorf("livemode = %v (%v), want test mode", live, err)
			}
		})
	}

	// The database is down while the key is checked: no verdict on the key,
	// so a retryable 503 that says nothing about the key or the database.
	t.Run("the database is down", func(t *testing.T) {
		down := testutil.OpenTracedDB(t, nil)
		_ = down.Pool.Close()
		rec, seen := serve(auth.NewService(down, audit.NewLogger(down)), keyRequest("Authorization", created.RawKey))
		body := rec.Body.String()
		if rec.Code != http.StatusServiceUnavailable || seen != nil {
			t.Fatalf("got %d %s (reached=%v), want 503", rec.Code, body, seen != nil)
		}
		if !strings.Contains(body, "authentication_unavailable") || strings.Contains(body, "invalid or expired") ||
			strings.Contains(body, "closed") || strings.Contains(body, "sql") {
			t.Errorf("body = %s, want authentication_unavailable and nothing about the key or the database", body)
		}
	})
}
