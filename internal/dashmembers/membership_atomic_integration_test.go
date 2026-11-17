package dashmembers_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/gethardcount/hardcount/internal/dashmembers"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/user"
)

// TestRemoveMember_ConcurrentRemovalsKeepOneMember: two members remove each
// other at the same moment, and one member remains. A workspace with no
// member has nobody who can sign in to it, and only psql can add one back.
//
// The test locks both membership rows, so the first removal waits for its
// caller's row and the second waits behind it for the per-tenant lock, then
// the test lets them go. The second removal starts after the first commits
// and is refused: its caller is no longer a member.
//
// Mutation: drop the caller's membership check from the removal, and both
// succeed: zero members. Drop the per-tenant lock, and one removal usually
// ends with a deadlock error where it should be refused.
func TestRemoveMember_ConcurrentRemovalsKeepOneMember(t *testing.T) {
	h := newHarness(t, "Team Mutual Removal")
	ctx := postgres.WithLivemode(context.Background(), false)

	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "second@example.com"); err != nil {
		t.Fatalf("invite: %v", err)
	}
	second, err := h.svc.AcceptInvite(ctx, dashmembers.AcceptInput{Token: h.email.lastToken(t), Password: testPassword})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	admin := testutil.AdminPool(t)
	gate, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin gate tx: %v", err)
	}
	defer func() { _ = gate.Rollback() }() // the release below already ended it on the normal path
	var locked int
	if err := gate.QueryRowContext(ctx, `
		SELECT count(*) FROM (SELECT 1 FROM user_tenants WHERE tenant_id = $1 FOR UPDATE) m`,
		h.tenantID).Scan(&locked); err != nil || locked != 2 {
		t.Fatalf("lock the membership rows: locked %d, err %v; want 2", locked, err)
	}

	results := make(chan error, 2)
	go func() { results <- h.svc.RemoveMember(ctx, h.tenantID, h.owner, second.UserID) }()
	go func() { results <- h.svc.RemoveMember(ctx, h.tenantID, second.UserID, h.owner) }()

	// Both removals wait on a lock: the gate's, or the other removal's.
	testutil.WaitForLockWaiters(t, ctx, admin, "", 2)
	if err := gate.Rollback(); err != nil {
		t.Fatalf("release gate: %v", err)
	}

	var removed, refused int
	for range 2 {
		switch err := <-results; {
		case err == nil:
			removed++
		case errors.Is(err, errs.ErrForbidden):
			refused++
		default:
			t.Errorf("removal: unexpected error %v", err)
		}
	}
	members, _, err := h.svc.List(ctx, h.tenantID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("members left after two members removed each other at once: got %d, want 1", len(members))
	}
	if removed != 1 || refused != 1 {
		t.Errorf("got %d removed and %d refused, want 1 and 1", removed, refused)
	}
}

// TestAcceptInvite_ConcurrentAcceptsIntoTwoWorkspacesKeepOneMembership: one
// account is invited to two workspaces and accepts both at the same moment.
// One accept joins, and the other is refused: an address signs in to one
// workspace, so the second membership could never be reached.
//
// The test locks the account's user row, so both accepts wait on it with
// their invitation claimed, then lets them go. The second starts its check
// after the first commits and sees the membership.
//
// Mutation: drop the user-row lock from AttachTenantInTx, and both accepts
// read no membership before either writes: two memberships.
func TestAcceptInvite_ConcurrentAcceptsIntoTwoWorkspacesKeepOneMembership(t *testing.T) {
	h := newHarness(t, "Team Two Accepts A")
	ctx := postgres.WithLivemode(context.Background(), false)
	second := testutil.CreateTestTenant(t, h.db, "Team Two Accepts B")
	secondOwner := createUser(t, h.db, "owner@two-accepts-b.example.com", second, "owner")
	const invitee = "both@example.com"
	u := accountInNoWorkspace(t, h, invitee)

	var tokens [2]string
	for i, at := range []struct{ tenantID, inviter string }{{h.tenantID, h.owner}, {second, secondOwner.ID}} {
		if _, err := h.svc.Invite(ctx, at.tenantID, at.inviter, invitee); err != nil {
			t.Fatalf("invite to workspace %d: %v", i+1, err)
		}
		tokens[i] = h.email.lastToken(t)
	}

	admin := testutil.AdminPool(t)
	gate, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin gate tx: %v", err)
	}
	defer func() { _ = gate.Rollback() }() // the release below already ended it on the normal path
	if _, err := gate.ExecContext(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, u.ID); err != nil {
		t.Fatalf("lock the user row: %v", err)
	}

	results := make(chan error, 2)
	for _, token := range tokens {
		go func() {
			_, err := h.svc.AcceptInvite(ctx, dashmembers.AcceptInput{Token: token})
			results <- err
		}()
	}

	// Both accepts wait on the gate: for the user row's lock, or, with that
	// lock gone, for the membership insert's foreign-key lock on the same row.
	testutil.WaitForLockWaiters(t, ctx, admin, "", 2)
	if err := gate.Rollback(); err != nil {
		t.Fatalf("release gate: %v", err)
	}

	var joined, refused int
	for range 2 {
		switch err := <-results; {
		case err == nil:
			joined++
		case errors.Is(err, user.ErrMemberElsewhere):
			refused++
		default:
			t.Errorf("accept: unexpected error %v", err)
		}
	}
	memberships, err := user.NewPostgresStore(h.db).TenantsForUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("tenants for user: %v", err)
	}
	if len(memberships) != 1 {
		t.Fatalf("memberships after one account accepted into two workspaces at once: got %d, want 1", len(memberships))
	}
	if joined != 1 || refused != 1 {
		t.Errorf("got %d joined and %d refused, want 1 and 1", joined, refused)
	}
}

// joinFailsDirectory is the test user directory with one fault: the
// membership write fails after the real one ran on the accept's transaction.
// The user row and the membership are on that transaction, so a test sees
// whether they roll back with the claim.
type joinFailsDirectory struct{ userDirAdapter }

func (d joinFailsDirectory) CreateInTx(ctx context.Context, tx *sql.Tx, address, passwordHash, tenantID, role string) (domain.User, error) {
	if _, err := d.userDirAdapter.CreateInTx(ctx, tx, address, passwordHash, tenantID, role); err != nil {
		return domain.User{}, err
	}
	return domain.User{}, errors.New("injected: membership attach failed")
}

func (d joinFailsDirectory) AttachTenantInTx(ctx context.Context, tx *sql.Tx, userID, tenantID, role string) error {
	if err := d.userDirAdapter.AttachTenantInTx(ctx, tx, userID, tenantID, role); err != nil {
		return err
	}
	return errors.New("injected: membership attach failed")
}

// TestAcceptInvite_FailedJoinKeepsTheInvite: when the membership or the
// audit row cannot be written, the accept leaves nothing behind. There is no
// user row and no session, and the invitation is still pending, so the
// invitee's retry works. A burned token means asking the operator for a new
// invite, and a user row with no membership would send the retry down the
// existing-account path, which signs nobody in.
//
// Mutation: commit the claim before join runs, and the invitation is used up.
func TestAcceptInvite_FailedJoinKeepsTheInvite(t *testing.T) {
	for _, tc := range []struct {
		name      string
		existing  bool
		failAudit bool
	}{
		{name: "new account"},
		{name: "existing account", existing: true},
		{name: "new account, audit row fails", failAudit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "Team Join Fails")
			ctx := postgres.WithLivemode(context.Background(), false)
			userStore := user.NewPostgresStore(h.db)
			const invitee = "joiner@example.com"
			if tc.existing {
				accountInNoWorkspace(t, h, invitee)
			}
			if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, invitee); err != nil {
				t.Fatalf("invite: %v", err)
			}
			token := h.email.lastToken(t)

			failing := dashmembers.NewService(dashmembers.NewPostgresStore(h.db),
				joinFailsDirectory{userDirAdapter{userStore}},
				h.sessions, h.audit, h.email, staticTenantNamer{name: "Team Join Fails"}, h.clk, "http://dash.test")
			if tc.failAudit {
				failing = failingAuditService(h)
			}
			if _, err := failing.AcceptInvite(ctx, dashmembers.AcceptInput{Token: token, Password: testPassword}); err == nil {
				t.Fatal("accept must fail when the membership cannot be written")
			}

			if !tc.existing {
				if _, err := userStore.GetByEmail(ctx, invitee); !errors.Is(err, errs.ErrNotFound) {
					t.Errorf("a failed accept left a user row (lookup err = %v)", err)
				}
			}
			if members, _, err := h.svc.List(ctx, h.tenantID); err != nil || len(members) != 1 {
				t.Errorf("members after the failed accept: got %d (err %v), want only the owner", len(members), err)
			}
			if n := countSessions(t, h.db, h.tenantID); n != 0 {
				t.Errorf("sessions in the workspace after the failed accept: got %d, want 0", n)
			}
			if _, err := h.svc.PreviewInvite(ctx, token); err != nil {
				t.Fatalf("a failed accept used up the invitation: %v", err)
			}
			res, err := h.svc.AcceptInvite(ctx, dashmembers.AcceptInput{Token: token, Password: testPassword})
			if err != nil {
				t.Fatalf("retry with a working directory: %v", err)
			}
			if res.NewAccount == tc.existing {
				t.Errorf("retry NewAccount = %v, want %v", res.NewAccount, !tc.existing)
			}
			members, _, err := h.svc.List(ctx, h.tenantID)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(members) != 2 {
				t.Errorf("members after the retry: got %d, want 2", len(members))
			}
		})
	}
}

// countSessions counts the session rows in tenantID.
func countSessions(t *testing.T, db *postgres.DB, tenantID string) int {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(t.Context(), `SELECT count(*) FROM dashboard_sessions WHERE tenant_id = $1`, tenantID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
