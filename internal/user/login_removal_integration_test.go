// This file is package user_test, not user, because it uses the real member
// removal: dashmembers imports user, so the internal test package cannot
// import dashmembers. It therefore repeats a few small helpers.
package user_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/dashmembers"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/session"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/user"
)

// A member removal during a sign-in, on real Postgres, with the real
// removal (dashmembers.PostgresStore.RemoveMember, which deletes the
// membership and ends the member's sessions in that workspace in one
// transaction). In either order, the removed member is left with no live
// session in the workspace.

const (
	removalMemberEmail    = "member@login-removal.test"
	removalMemberPassword = "login-removal-pw-12"
)

type removalIT struct {
	db       *postgres.DB
	tenantID string
	memberID string
	sessSvc  *session.Service
}

func newRemovalIT(t *testing.T) removalIT {
	t.Helper()
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Login Removal")
	// The owner stays, so the member is not the workspace's last member.
	user.CreateTestUser(t, db, "owner@login-removal.test", "login-removal-owner-pw", tenantID, "owner")
	m := user.CreateTestUser(t, db, removalMemberEmail, removalMemberPassword, tenantID, "member")
	return removalIT{db: db, tenantID: tenantID, memberID: m.ID, sessSvc: session.NewService(session.NewPostgresStore(db))}
}

// remove removes the member the way the members route does: the
// membership, then the member's sessions in the workspace, in one
// transaction.
func (it removalIT) remove(ctx context.Context) error {
	return it.removeHolding(ctx, nil)
}

// removeHolding is remove, waiting at gate (when not nil) inside the
// removal's transaction, after its DELETE and before the sign-out.
func (it removalIT) removeHolding(ctx context.Context, gate *user.Gate) error {
	return dashmembers.NewPostgresStore(it.db).RemoveMember(postgres.WithLivemode(ctx, false), it.tenantID, it.memberID, time.Now().UTC(),
		func(tx *sql.Tx, _ []string) error {
			if gate != nil {
				gate.Hold()
			}
			return it.sessSvc.EndForMemberTx(ctx, tx, it.memberID, it.tenantID)
		})
}

// joinSecondWorkspace makes the member join a second workspace, after the
// first, and returns it.
func (it removalIT) joinSecondWorkspace(t *testing.T) string {
	t.Helper()
	second := testutil.CreateTestTenant(t, it.db, "Login Removal Second")
	tx, err := it.db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	if err := user.NewPostgresStore(it.db).AttachTenantInTx(t.Context(), tx, it.memberID, second, "member"); err != nil {
		t.Fatalf("join the second workspace: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return second
}

// signInMember posts the member's sign-in form to h.
func signInMember(h http.Handler) *httptest.ResponseRecorder {
	body, _ := json.Marshal(user.LoginReq{Email: removalMemberEmail, Password: removalMemberPassword})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(string(body))))
	return rr
}

func (it removalIT) handler(store user.Store) http.Handler {
	return user.NewHandler(it.service(store, audit.NewLogger(it.db)), it.sessSvc,
		session.CookieConfig{Path: "/"}, nil, "", false).Routes()
}

// liveSessions counts the member's live sessions in the workspace, read
// outside row-level security.
func (it removalIT) liveSessions(t *testing.T) int {
	t.Helper()
	tx, err := it.db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(t.Context(), `
		SELECT count(*) FROM dashboard_sessions
		WHERE user_id = $1 AND tenant_id = $2 AND revoked_at IS NULL`, it.memberID, it.tenantID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (it removalIT) service(store user.Store, auditor user.AuditTxWriter) *user.Service {
	return user.NewService(store, clock.Real(), it.sessSvc, auditor)
}

// removeAfterLookup is the real store, removing the member once after
// GetByEmail returns: after the sign-in read the account and before its
// transaction.
type removeAfterLookup struct {
	user.Store
	it      removalIT
	t       *testing.T
	removed bool
}

func (r *removeAfterLookup) GetByEmail(ctx context.Context, email string) (domain.User, error) {
	u, err := r.Store.GetByEmail(ctx, email)
	if !r.removed {
		r.removed = true
		if err := r.it.remove(ctx); err != nil {
			r.t.Errorf("remove the member during the sign-in: %v", err)
		}
	}
	return u, err
}

// holdAfterSession is the real audit writer that, inside the sign-in's
// transaction (after the session row is written), waits at gate before it
// writes the row.
type holdAfterSession struct {
	real *audit.Logger
	gate *user.Gate
}

func (h holdAfterSession) LogInTx(ctx context.Context, tx *sql.Tx, e audit.Entry) error {
	h.gate.Hold()
	return h.real.LogInTx(ctx, tx, e)
}

// TestLogin_NoWorkspace: a member removed after the sign-in read the account
// gets the no-membership refusal and no session. A removal that starts while
// a sign-in's transaction holds the membership waits for it, then ends the
// session that sign-in created. A sign-in that starts while a removal's
// transaction holds the membership waits for it, then signs in to the
// member's next workspace, or is refused when there is none.
func TestLogin_NoWorkspace(t *testing.T) {
	t.Run("removed before the sign-in's transaction", func(t *testing.T) {
		it := newRemovalIT(t)
		store := &removeAfterLookup{Store: user.NewPostgresStore(it.db), it: it, t: t}
		rr := signInMember(it.handler(store))

		if rr.Code != http.StatusConflict {
			t.Fatalf("sign-in of a removed member: status %d, want 409; body %s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "user has no tenant membership; contact your administrator") {
			t.Errorf("body = %s, want the no-membership refusal", rr.Body.String())
		}
		if !store.removed {
			t.Fatal("fixture: the member was not removed")
		}
		if n := it.liveSessions(t); n != 0 {
			t.Errorf("live sessions in the workspace = %d, want 0", n)
		}
	})

	t.Run("removed while the sign-in's transaction holds the membership", func(t *testing.T) {
		it := newRemovalIT(t)
		gate := user.NewGate(t)
		svc := it.service(user.NewPostgresStore(it.db), holdAfterSession{real: audit.NewLogger(it.db), gate: gate})

		type loginOutcome struct {
			res user.LoginResult
			err error
		}
		loginDone := make(chan loginOutcome, 1)
		go func() {
			res, err := svc.Login(context.Background(), user.LoginInput{Email: removalMemberEmail, Password: removalMemberPassword})
			loginDone <- loginOutcome{res, err}
		}()
		gate.WaitReached(t)

		removeDone := make(chan error, 1)
		go func() { removeDone <- it.remove(context.Background()) }()

		// The removal's DELETE waits on the membership row the sign-in locked.
		user.WaitForLockWait(t, it.db, "%DELETE FROM user_tenants%")
		select {
		case err := <-removeDone:
			t.Fatalf("the removal finished while the sign-in held the membership: err %v", err)
		default:
		}

		gate.Open()
		out := user.Receive(t, loginDone, "the sign-in")
		if out.err != nil {
			t.Fatalf("sign-in: %v, want it to commit", out.err)
		}
		if err := user.Receive(t, removeDone, "the removal"); err != nil {
			t.Fatalf("removal: %v, want it to commit after the sign-in", err)
		}
		if _, err := it.sessSvc.Resolve(t.Context(), out.res.RawSessionID); !errors.Is(err, session.ErrNotFound) {
			t.Errorf("resolve the session the sign-in created: err = %v, want session.ErrNotFound", err)
		}
		if n := it.liveSessions(t); n != 0 {
			t.Errorf("live sessions in the workspace = %d, want 0", n)
		}
	})

	// A sign-in that starts while a removal's transaction holds the deleted
	// membership waits for it. After the removal commits, the read of the
	// first-joined membership skips the deleted row and returns the next
	// one, if any.
	waitForRemoval := func(t *testing.T, it removalIT) *httptest.ResponseRecorder {
		t.Helper()
		gate := user.NewGate(t)
		removeDone := make(chan error, 1)
		go func() { removeDone <- it.removeHolding(context.Background(), gate) }()
		gate.WaitReached(t)

		signInDone := make(chan *httptest.ResponseRecorder, 1)
		h := it.handler(user.NewPostgresStore(it.db))
		go func() { signInDone <- signInMember(h) }()

		user.WaitForLockWait(t, it.db, "%FROM user_tenants%")
		gate.Open()
		if err := user.Receive(t, removeDone, "the removal"); err != nil {
			t.Fatalf("removal: %v", err)
		}
		return user.Receive(t, signInDone, "the sign-in")
	}

	t.Run("sign-in waits for a removal: one workspace", func(t *testing.T) {
		it := newRemovalIT(t)
		rr := waitForRemoval(t, it)
		if rr.Code != http.StatusConflict {
			t.Fatalf("sign-in after the removal: status %d, want 409; body %s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "user has no tenant membership; contact your administrator") {
			t.Errorf("body = %s, want the no-membership refusal", rr.Body.String())
		}
		if n := it.liveSessions(t); n != 0 {
			t.Errorf("live sessions in the workspace = %d, want 0", n)
		}
	})

	t.Run("sign-in waits for a removal: two workspaces", func(t *testing.T) {
		it := newRemovalIT(t)
		second := it.joinSecondWorkspace(t)
		rr := waitForRemoval(t, it)
		if rr.Code != http.StatusOK {
			t.Fatalf("sign-in after the removal: status %d, want 200; body %s", rr.Code, rr.Body.String())
		}
		var resp user.LoginResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.TenantID != second {
			t.Errorf("signed in to %s, want the second workspace %s", resp.TenantID, second)
		}
		if n := it.liveSessions(t); n != 0 {
			t.Errorf("live sessions in the removed workspace = %d, want 0", n)
		}
		for _, c := range rr.Result().Cookies() {
			if c.Name != session.CookieName {
				continue
			}
			sess, err := it.sessSvc.Resolve(t.Context(), c.Value)
			if err != nil || sess.TenantID != second {
				t.Errorf("resolve the new session: tenant %q, err %v; want a live session in %s", sess.TenantID, err, second)
			}
		}
	})
}
