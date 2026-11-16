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
	return dashmembers.NewPostgresStore(it.db).RemoveMember(postgres.WithLivemode(ctx, false), it.tenantID, it.memberID, time.Now().UTC(),
		func(tx *sql.Tx, _ []string) error {
			return it.sessSvc.EndForMemberTx(ctx, tx, it.memberID, it.tenantID)
		})
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
// transaction (after the session row is written), says so on ready and
// waits for release before it writes the row.
type holdAfterSession struct {
	real    *audit.Logger
	ready   chan struct{}
	release chan struct{}
}

func (h holdAfterSession) LogInTx(ctx context.Context, tx *sql.Tx, e audit.Entry) error {
	close(h.ready)
	<-h.release
	return h.real.LogInTx(ctx, tx, e)
}

// TestLogin_NoWorkspace: a member removed after the sign-in read the account
// gets the no-membership refusal and no session. And the other order: a
// removal that starts while a sign-in's transaction holds the membership
// waits for it, then ends the session that sign-in created.
func TestLogin_NoWorkspace(t *testing.T) {
	t.Run("removed before the sign-in's transaction", func(t *testing.T) {
		it := newRemovalIT(t)
		store := &removeAfterLookup{Store: user.NewPostgresStore(it.db), it: it, t: t}
		h := user.NewHandler(it.service(store, audit.NewLogger(it.db)), it.sessSvc,
			session.CookieConfig{Path: "/"}, nil, "", false)

		body, _ := json.Marshal(user.LoginReq{Email: removalMemberEmail, Password: removalMemberPassword})
		rr := httptest.NewRecorder()
		h.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(string(body))))

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
		hold := holdAfterSession{real: audit.NewLogger(it.db), ready: make(chan struct{}), release: make(chan struct{})}
		svc := it.service(user.NewPostgresStore(it.db), hold)

		type loginOutcome struct {
			res user.LoginResult
			err error
		}
		loginDone := make(chan loginOutcome, 1)
		go func() {
			res, err := svc.Login(context.Background(), user.LoginInput{Email: removalMemberEmail, Password: removalMemberPassword})
			loginDone <- loginOutcome{res, err}
		}()
		select {
		case <-hold.ready:
		case out := <-loginDone:
			t.Fatalf("the sign-in finished before its audit row: %+v", out)
		}

		removeDone := make(chan error, 1)
		go func() { removeDone <- it.remove(context.Background()) }()

		// The removal's DELETE waits on the membership row the sign-in
		// locked. pg_stat_activity shows it waiting on a lock.
		deadline := time.Now().Add(10 * time.Second)
		for {
			var waiting bool
			if err := it.db.Pool.QueryRowContext(t.Context(), `
				SELECT EXISTS (SELECT 1 FROM pg_stat_activity
				WHERE datname = current_database() AND wait_event_type = 'Lock'
				  AND query LIKE '%DELETE FROM user_tenants%')`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting {
				break
			}
			select {
			case err := <-removeDone:
				close(hold.release)
				t.Fatalf("the removal finished while the sign-in held the membership: err %v", err)
			default:
			}
			if time.Now().After(deadline) {
				close(hold.release)
				t.Fatal("the removal never waited on the sign-in's lock")
			}
		}

		close(hold.release)
		out := <-loginDone
		if out.err != nil {
			t.Fatalf("sign-in: %v, want it to commit", out.err)
		}
		if err := <-removeDone; err != nil {
			t.Fatalf("removal: %v, want it to commit after the sign-in", err)
		}
		if _, err := it.sessSvc.Resolve(t.Context(), out.res.RawSessionID); !errors.Is(err, session.ErrNotFound) {
			t.Errorf("resolve the session the sign-in created: err = %v, want session.ErrNotFound", err)
		}
		if n := it.liveSessions(t); n != 0 {
			t.Errorf("live sessions in the workspace = %d, want 0", n)
		}
	})
}
