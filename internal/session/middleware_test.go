package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

// canceledStore fails every lookup the way the Postgres store does when the
// request's context is cancelled mid-query.
type canceledStore struct{ fakeSessionStore }

func (*canceledStore) GetByIDHash(context.Context, string) (Session, error) {
	return Session{}, fmt.Errorf("get session: %w", context.Canceled)
}

// TestMiddlewareOrAPIKey_ClientGoneLogsAtWarn: when the session lookup fails
// because the client went away (a closed browser tab), the 503 is logged at
// WARN. The same error on a live request stays at ERROR, the level operators
// alert on.
func TestMiddlewareOrAPIKey_ClientGoneLogsAtWarn(t *testing.T) {
	svc := NewService(&canceledStore{})
	handler := MiddlewareOrAPIKey(svc, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not reach handler")
	}))
	gone, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tc := range []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"client went away", gone, "WARN"},
		{"request still live", context.Background(), "ERROR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			handler.ServeHTTP(httptest.NewRecorder(), sessionRequest("sess_deadbeef").WithContext(tc.ctx))

			var entry map[string]any
			if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
				t.Fatalf("want exactly one JSON log line, got %q: %v", buf.String(), err)
			}
			if entry["level"] != tc.want || entry["msg"] != "session resolve unavailable" {
				t.Errorf("logged %v %q, want %s %q", entry["level"], entry["msg"], tc.want, "session resolve unavailable")
			}
		})
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
