package session

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/platform/request"
	"github.com/gethardcount/hardcount/internal/platform/request/requesttest"
)

// fakeSessionStore serves the sessions it holds by hash.
type fakeSessionStore struct {
	sessions map[string]Session
}

func (f *fakeSessionStore) InsertTx(context.Context, *sql.Tx, Session) error { return nil }
func (f *fakeSessionStore) EndTx(context.Context, *sql.Tx, string) (bool, error) {
	return false, nil
}
func (f *fakeSessionStore) GetByIDHash(_ context.Context, idHash string, _ time.Time) (Session, error) {
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

// canceledStore fails every lookup the way the Postgres store does when the
// request's context is cancelled mid-query.
type canceledStore struct{ fakeSessionStore }

func (*canceledStore) GetByIDHash(context.Context, string, time.Time) (Session, error) {
	return Session{}, fmt.Errorf("get session: %w", context.Canceled)
}

// A session store that fails is no verdict on the cookie: the request stops
// with a 503 and does not go on to the API key, where with no key it would
// get a 401 for missing credentials. When the client went away (a closed
// browser tab), the 503 is logged at WARN. The same error on a live request
// stays at ERROR, the level operators alert on.
func TestRequireCaller_SessionStoreFails(t *testing.T) {
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

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, sessionRequest("sess_deadbeef").WithContext(tc.ctx))

			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503 (no fall-through to the API key)", rec.Code)
			}
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

// The access log wraps RequireCaller and cannot see the context it hands its
// handler, and sign-out and the mode switch resolve the cookie themselves. So
// RequireCaller and NameActor name a session's user on the request's record,
// under the names the audit log gives the same session's context.
func TestSessionUserNamedOnTheRecord(t *testing.T) {
	store := &fakeSessionStore{sessions: map[string]Session{}}
	sessions := NewService(store)
	rawSession, sess, err := sessions.IssueTx(t.Context(), nil, IssueInput{UserID: "usr_owner", TenantID: "ten_1"})
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	store.sessions[sess.IDHash] = sess // the fake's InsertTx keeps nothing
	want := request.Actor{Type: "user", ID: "usr_owner", TenantID: "ten_1"}

	var seen *http.Request
	for name, h := range map[string]http.Handler{
		"RequireCaller": RequireCaller(sessions, nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = r })),
		"NameActor":     http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { NameActor(r.Context(), sess) }),
	} {
		record, rec := recordOf(h, sessionRequest(rawSession))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", name, rec.Code)
		}
		if got := record.Actor(); got != want {
			t.Errorf("%s: actor on the record = %+v, want %+v", name, got, want)
		}
	}
	if actorType, actorID := audit.ResolveActor(seen.Context()); actorType != want.Type || actorID != want.ID {
		t.Errorf("the audit log names this session %s/%s, the record %s/%s", actorType, actorID, want.Type, want.ID)
	}

	// No credential: 401 for missing credentials, the text the API docs
	// promise. A key is read from Authorization: Bearer only, so one in
	// X-API-Key is none (with a nil key service, reading it would panic).
	keyInOtherHeader := httptest.NewRequest("GET", "/", nil)
	keyInOtherHeader.Header.Set("X-API-Key", "hc_secret_test_"+strings.Repeat("0", 64))
	for name, req := range map[string]*http.Request{"unknown cookie": sessionRequest("sess_unknown"), "key in X-API-Key": keyInOtherHeader} {
		seen = nil
		record, rec := recordOf(RequireCaller(sessions, nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = r })), req)
		if rec.Code != http.StatusUnauthorized || seen != nil || !strings.Contains(rec.Body.String(), "missing credentials") {
			t.Errorf("%s: got %d %s (reached=%v), want 401 missing credentials", name, rec.Code, rec.Body.String(), seen != nil)
		}
		if got := record.Actor(); got != (request.Actor{}) {
			t.Errorf("%s: actor on the record = %+v, want none: nobody signed in", name, got)
		}
	}
}

// recordOf serves req through h under the server's root, so the request has
// its record, and returns the record and the response.
func recordOf(h http.Handler, req *http.Request) (*request.Record, *httptest.ResponseRecorder) {
	var record *request.Record
	w := httptest.NewRecorder()
	requesttest.Root(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record = request.Of(r.Context())
		h.ServeHTTP(w, r)
	})).ServeHTTP(w, req)
	return record, w
}
