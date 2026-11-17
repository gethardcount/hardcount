package session

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/platform/request"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// The API key half of RequireCaller, on real Postgres: what a request with a
// key that is not valid, a valid key, a valid key beside a stale session
// cookie, and a key checked while the database is down each get.

// keyRequest is a request with no cookie, carrying key as a Bearer token.
func keyRequest(key string) *http.Request {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	return req
}

func TestRequireCaller_APIKey(t *testing.T) {
	db := testutil.SetupTestDB(t)
	keys := auth.NewService(db, audit.NewLogger(db))
	tenantID := testutil.CreateTestTenant(t, db, "Session API Key")
	created, err := keys.CreateKey(postgres.WithLivemode(t.Context(), false), tenantID, auth.CreateKeyInput{Name: "CI", KeyType: auth.KeyTypeSecret})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	sessions := NewService(&fakeSessionStore{sessions: map[string]Session{}})

	serve := func(svc *auth.Service, req *http.Request) (*httptest.ResponseRecorder, *http.Request, *request.Record) {
		t.Helper()
		var seen *http.Request
		record, rec := recordOf(RequireCaller(sessions, svc)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			seen = r
		})), req)
		return rec, seen, record
	}

	t.Run("a key that is not valid", func(t *testing.T) {
		rec, seen, _ := serve(keys, keyRequest("hc_secret_test_"+strings.Repeat("0", 64)))
		if rec.Code != http.StatusUnauthorized || seen != nil || !strings.Contains(rec.Body.String(), "invalid or expired API key") {
			t.Errorf("got %d %s (reached=%v), want 401 with the generic message", rec.Code, rec.Body.String(), seen != nil)
		}
	})

	t.Run("a valid key", func(t *testing.T) {
		rec, seen, record := serve(keys, keyRequest(created.RawKey))
		if rec.Code != http.StatusOK || seen == nil {
			t.Fatalf("got %d %s, want 200", rec.Code, rec.Body.String())
		}
		ctx := seen.Context()
		want := auth.Caller{TenantID: tenantID, KeyID: created.Key.ID, KeyType: auth.KeyTypeSecret}
		if got := auth.CallerOf(ctx); got != want {
			t.Errorf("caller = %+v, want %+v", got, want)
		}
		if live, err := postgres.Livemode(ctx); err != nil || live {
			t.Errorf("livemode = %v (%v), want test mode", live, err)
		}
		// The layer above reads the caller from the request's record.
		onRecord := request.Actor{Type: "api_key", ID: created.Key.ID, TenantID: tenantID}
		if got := record.Actor(); got != onRecord {
			t.Errorf("actor on the record = %+v, want %+v", got, onRecord)
		}
	})

	// A cookie that names no live session does not block a valid key on the
	// same request: the request is the key's.
	t.Run("a stale cookie and a valid key", func(t *testing.T) {
		req := keyRequest(created.RawKey)
		req.AddCookie(&http.Cookie{Name: CookieName, Value: "sess_ended"})
		rec, seen, _ := serve(keys, req)
		if rec.Code != http.StatusOK || seen == nil {
			t.Fatalf("got %d %s, want 200: the stale cookie blocked the key", rec.Code, rec.Body.String())
		}
		if c := auth.CallerOf(seen.Context()); c.TenantID != tenantID || c.KeyID != created.Key.ID {
			t.Errorf("principal = tenant %q, key %q; want the key's %s, %s", c.TenantID, c.KeyID, tenantID, created.Key.ID)
		}
	})

	// The database is down while the key is checked: no verdict on the key,
	// so a retryable 503 that says nothing about the key or the database.
	t.Run("the database is down", func(t *testing.T) {
		down := testutil.OpenTracedDB(t, nil)
		_ = down.Pool.Close()
		rec, seen, _ := serve(auth.NewService(down, audit.NewLogger(down)), keyRequest(created.RawKey))
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

// The context Authenticate returns names one caller, whatever the request's
// context already held: a key with no user, or a session with no key. The
// event stream re-runs Authenticate on a request whose context holds the
// caller that opened it, and compares the two.
func TestAuthenticate_NamesOneCaller(t *testing.T) {
	db := testutil.SetupTestDB(t)
	keys := auth.NewService(db, audit.NewLogger(db))
	tenantID := testutil.CreateTestTenant(t, db, "One Caller")
	created, err := keys.CreateKey(postgres.WithLivemode(t.Context(), false), tenantID, auth.CreateKeyInput{Name: "CI"})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	store := &fakeSessionStore{sessions: map[string]Session{}}
	sessions := NewService(store)
	rawSession, sess, err := sessions.IssueTx(t.Context(), nil, IssueInput{UserID: "usr_owner", TenantID: tenantID})
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	store.sessions[sess.IDHash] = sess // the fake's InsertTx keeps nothing
	held := auth.WithCaller(t.Context(), auth.Caller{UserID: "usr_held", KeyID: "hc_key_held"})

	t.Run("a key", func(t *testing.T) {
		ctx, err := Authenticate(keyRequest(created.RawKey).WithContext(held), sessions, keys)
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		if got := auth.CallerOf(ctx).KeyID; got != created.Key.ID {
			t.Errorf("key id = %q, want %q", got, created.Key.ID)
		}
		if got := auth.CallerOf(ctx).UserID; got != "" {
			t.Errorf("user id = %q, want none for a key", got)
		}
	})

	// The store refuses a session that is not live, in its SQL, at the instant
	// its caller passes: one expiring at that instant is expired.
	t.Run("an expired session", func(t *testing.T) {
		store := NewPostgresStore(db)
		expires := time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond)
		tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		var userID string
		if err := tx.QueryRowContext(t.Context(), `INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
			"expired-"+tenantID+"@example.com").Scan(&userID); err != nil {
			t.Fatalf("insert user: %v", err)
		}
		expired := Session{IDHash: HashID("sess_expired"), UserID: userID, TenantID: tenantID, CreatedAt: expires.Add(-time.Hour), ExpiresAt: expires}
		if err := store.InsertTx(t.Context(), tx, expired); err != nil {
			t.Fatalf("insert session: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}

		req := httptest.NewRequest("GET", "/", nil)
		req.AddCookie(&http.Cookie{Name: CookieName, Value: "sess_expired"})
		if _, err := Authenticate(req, NewService(store), keys); !errors.Is(err, ErrNoCredentials) {
			t.Errorf("an expired session: err = %v, want ErrNoCredentials", err)
		}
		if _, err := store.GetByIDHash(t.Context(), expired.IDHash, expires); !errors.Is(err, ErrNotFound) {
			t.Errorf("at its expiry instant: err = %v, want ErrNotFound", err)
		}
		if _, err := store.GetByIDHash(t.Context(), expired.IDHash, expires.Add(-time.Microsecond)); err != nil {
			t.Errorf("a microsecond before its expiry: err = %v, want the session", err)
		}
	})

	t.Run("a session", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil).WithContext(held)
		req.AddCookie(&http.Cookie{Name: CookieName, Value: rawSession})
		ctx, err := Authenticate(req, sessions, keys)
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		if got := auth.CallerOf(ctx).UserID; got != sess.UserID {
			t.Errorf("user id = %q, want %q", got, sess.UserID)
		}
		if got := auth.CallerOf(ctx).KeyID; got != "" {
			t.Errorf("key id = %q, want none for a session", got)
		}
	})
}
