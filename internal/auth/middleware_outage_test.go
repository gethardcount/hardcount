package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// outageStore simulates a store whose backing DB is unreachable: every
// lookup errors with an infrastructure failure, never a verdict.
type outageStore struct{ Store }

func (o *outageStore) GetByPrefix(context.Context, string) (domain.APIKey, error) {
	return domain.APIKey{}, errors.New("checkout conn: dial tcp 127.0.0.1:5432: connect: connection refused")
}
func (o *outageStore) TouchLastUsed(context.Context, string, time.Time) error { return nil }

// TestMiddleware_StoreOutageIs503NotInvalidKey is the #560 regression: a DB
// outage during key validation must surface as a retryable 503, never the
// 401 "invalid or expired API key" that sends integrators into a pointless
// credential rotation. Reverting ValidateKey to a blanket "invalid api key"
// turns this red.
func TestMiddleware_StoreOutageIs503NotInvalidKey(t *testing.T) {
	svc := NewService(&outageStore{Store: newMemStore()})
	mw := Middleware(svc)

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not reach handler")
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer hc_secret_test_0000000000000000000000000000000000000000000000000000000000000000")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503 — an infra failure is not a credential verdict", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "invalid or expired") {
		t.Errorf("body claims the key is invalid during an outage: %s", body)
	}
	if !strings.Contains(body, "authentication_unavailable") {
		t.Errorf("body missing the retryable code: %s", body)
	}
	if strings.Contains(body, "5432") || strings.Contains(body, "connection refused") {
		t.Errorf("body leaks DB internals (ADR-026): %s", body)
	}
}

// TestMiddleware_UnknownKeyStays401 pins the other half of the split: a key
// the store definitively does not have keeps the generic 401 (ADR-026
// anti-enumeration), proving the outage branch didn't widen.
func TestMiddleware_UnknownKeyStays401(t *testing.T) {
	svc := NewService(newMemStore())
	mw := Middleware(svc)

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not reach handler")
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer hc_secret_test_0000000000000000000000000000000000000000000000000000000000000000")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid or expired API key") {
		t.Errorf("generic message changed: %s", rec.Body.String())
	}
}
