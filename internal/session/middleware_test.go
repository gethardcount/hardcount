package session

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSessionStore serves one session by hash, or fails every lookup with an
// infrastructure error when outage is set.
type fakeSessionStore struct {
	sessions map[string]Session
	outage   bool
}

func (f *fakeSessionStore) Insert(context.Context, Session) error { return nil }
func (f *fakeSessionStore) GetByIDHash(_ context.Context, idHash string) (Session, error) {
	if f.outage {
		return Session{}, errors.New("checkout conn: dial tcp 127.0.0.1:5432: connect: connection refused")
	}
	s, ok := f.sessions[idHash]
	if !ok {
		return Session{}, ErrNotFound
	}
	return s, nil
}
func (f *fakeSessionStore) Revoke(context.Context, string) error               { return nil }
func (f *fakeSessionStore) RevokeAllForUser(context.Context, string) error     { return nil }
func (f *fakeSessionStore) UpdateLivemode(context.Context, string, bool) error { return nil }

func sessionRequest(cookie string) *http.Request {
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: cookie})
	return req
}

// TestMiddlewareOrAPIKey_CookieOutageDoesNotFallThrough: pre-#560 an infra
// failure on the cookie lookup fell through to the bearer branch and, with
// no header present, surfaced as "missing credentials" — a second flavor of
// the same lie. The outage must stop the request with a 503.
func TestMiddlewareOrAPIKey_CookieOutageDoesNotFallThrough(t *testing.T) {
	svc := NewService(&fakeSessionStore{outage: true})
	handler := MiddlewareOrAPIKey(svc, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not reach handler")
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, sessionRequest("sess_deadbeef"))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503 (no fall-through on outage)", rec.Code)
	}
}

// TestMiddlewareOrAPIKey_StaleCookieStillFallsThrough pins the deliberate
// pre-existing behavior the outage branch must not widen: a cookie the store
// definitively rejects falls through to the bearer branch (here: absent →
// the generic missing-credentials 401), so a stale cookie never blocks a
// valid Authorization header.
func TestMiddlewareOrAPIKey_StaleCookieStillFallsThrough(t *testing.T) {
	svc := NewService(&fakeSessionStore{sessions: map[string]Session{}})
	handler := MiddlewareOrAPIKey(svc, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not reach handler")
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, sessionRequest("sess_deadbeef"))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401 from the bearer branch", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "missing credentials") {
		t.Errorf("expected the bearer branch's missing-credentials message, got: %s", rec.Body.String())
	}
}
