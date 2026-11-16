package session

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/platform/request"
)

// fakeSessionStore serves one session by hash, or fails every lookup with an
// infrastructure error when outage is set.
type fakeSessionStore struct {
	sessions map[string]Session
	outage   bool
}

func (f *fakeSessionStore) InsertTx(context.Context, *sql.Tx, Session) error { return nil }
func (f *fakeSessionStore) RevokeTx(context.Context, *sql.Tx, string) (bool, error) {
	return false, nil
}
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
func (f *fakeSessionStore) EndAllForUserTx(context.Context, *sql.Tx, string) error { return nil }
func (f *fakeSessionStore) EndForMemberTx(context.Context, *sql.Tx, string, string) error {
	return nil
}
func (f *fakeSessionStore) EndWith(context.Context, string, func(*sql.Tx, Session) error) (Session, bool, error) {
	return Session{}, false, nil
}
func (f *fakeSessionStore) SetLivemodeWith(context.Context, string, bool, func(*sql.Tx, Session) error) (Session, bool, error) {
	return Session{}, false, nil
}

func sessionRequest(cookie string) *http.Request {
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: cookie})
	return req
}

// A session store that fails is no verdict on the cookie: the request stops
// with a 503 and does not go on to the API key, where with no key it would
// get a 401 for missing credentials.
func TestRequireCaller_CookieOutageDoesNotFallThrough(t *testing.T) {
	svc := NewService(&fakeSessionStore{outage: true})
	handler := RequireCaller(svc, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

// TestRequireCaller_ClientGoneLogsAtWarn: when the session lookup fails
// because the client went away (a closed browser tab), the 503 is logged at
// WARN. The same error on a live request stays at ERROR, the level operators
// alert on.
func TestRequireCaller_ClientGoneLogsAtWarn(t *testing.T) {
	svc := NewService(&canceledStore{})
	handler := RequireCaller(svc, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

// The access log wraps RequireCaller and cannot see the context it hands
// its handler. So the middleware names the caller it accepted on the request's
// record, under the names the audit log gives the same context.
func TestRequireCaller_NamesTheCallerOnTheRecord(t *testing.T) {
	store := &fakeSessionStore{sessions: map[string]Session{}}
	sessions := NewService(store)
	rawSession, sess, err := sessions.IssueTx(t.Context(), nil, IssueInput{UserID: "usr_owner", TenantID: "ten_1"})
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	store.sessions[sess.IDHash] = sess // the fake's InsertTx keeps nothing

	serve := func(cookie string) (outer context.Context, seen *http.Request, status int) {
		outer = request.WithRecord(t.Context())
		rec := httptest.NewRecorder()
		RequireCaller(sessions, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = r
		})).ServeHTTP(rec, sessionRequest(cookie).WithContext(outer))
		return outer, seen, rec.Code
	}

	t.Run("a session", func(t *testing.T) {
		outer, seen, status := serve(rawSession)
		if status != http.StatusOK || seen == nil {
			t.Fatalf("status = %d (reached=%v), want 200", status, seen != nil)
		}
		want := request.Caller{ActorType: "user", ActorID: "usr_owner", TenantID: "ten_1"}
		if got := request.CallerOf(outer); got != want {
			t.Errorf("caller on the record = %+v, want %+v", got, want)
		}
		if actorType, actorID := audit.ResolveActor(seen.Context()); actorType != want.ActorType || actorID != want.ActorID {
			t.Errorf("the audit log names this caller %s/%s, the record %s/%s", actorType, actorID, want.ActorType, want.ActorID)
		}
	})

	// An unknown cookie and no API key: 401 for missing credentials.
	t.Run("refused", func(t *testing.T) {
		outer, seen, status := serve("sess_unknown")
		if status != http.StatusUnauthorized || seen != nil {
			t.Fatalf("status = %d (reached=%v), want 401", status, seen != nil)
		}
		if got := request.CallerOf(outer); got != (request.Caller{}) {
			t.Errorf("caller on the record = %+v, want none: nobody signed in", got)
		}
	})
}

// callerIn and the audit log name an API key's context the same way.
func TestCallerIn_APIKey(t *testing.T) {
	ctx := auth.WithKeyID(auth.WithUserID(auth.WithTenantID(t.Context(), "ten_1"), ""), "hc_key_9")
	want := request.Caller{ActorType: "api_key", ActorID: "hc_key_9", TenantID: "ten_1"}
	if got := callerIn(ctx); got != want {
		t.Errorf("callerIn = %+v, want %+v", got, want)
	}
	if actorType, actorID := audit.ResolveActor(ctx); actorType != want.ActorType || actorID != want.ActorID {
		t.Errorf("the audit log names this caller %s/%s, callerIn %s/%s", actorType, actorID, want.ActorType, want.ActorID)
	}
}

// NameCaller and the audit log name a session's user the same way.
func TestNameCaller(t *testing.T) {
	ctx := request.WithRecord(t.Context())
	sess := Session{UserID: "usr_owner", TenantID: "ten_1"}
	NameCaller(ctx, sess)
	want := request.Caller{ActorType: "user", ActorID: "usr_owner", TenantID: "ten_1"}
	if got := request.CallerOf(ctx); got != want {
		t.Errorf("caller on the record = %+v, want %+v", got, want)
	}
	if actorType, actorID := audit.ResolveActor(applyToCtx(t.Context(), sess)); actorType != want.ActorType || actorID != want.ActorID {
		t.Errorf("the audit log names this session %s/%s, NameCaller %s/%s", actorType, actorID, want.ActorType, want.ActorID)
	}
}
