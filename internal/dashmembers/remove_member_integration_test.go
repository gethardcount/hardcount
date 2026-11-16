package dashmembers_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/dashmembers"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/session"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/user"
)

// removalCtx is the context the session middleware gives the owner's request:
// workspace, actor and test mode.
func removalCtx(h *harness) context.Context {
	ctx := auth.WithTenantID(context.Background(), h.tenantID)
	ctx = auth.WithUserID(ctx, h.owner)
	return postgres.WithLivemode(ctx, false)
}

// attachMember adds an existing user to another workspace.
func attachMember(t *testing.T, db *postgres.DB, userID, tenantID string) {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	if err := user.NewPostgresStore(db).AttachTenantInTx(t.Context(), tx, userID, tenantID, "member"); err != nil {
		t.Fatalf("attach %s to %s: %v", userID, tenantID, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// issue mints a session for userID in tenantID and mode and returns its
// cookie value.
func issue(t *testing.T, h *harness, userID, tenantID string, live bool) string {
	t.Helper()
	raw, _, err := h.sessions.Issue(context.Background(), session.IssueInput{UserID: userID, TenantID: tenantID, Livemode: live})
	if err != nil {
		t.Fatalf("issue a session for %s in %s (live=%v): %v", userID, tenantID, live, err)
	}
	return raw
}

// invitationRevokedAt reads the revoked_at of the tenant's invitation to
// address.
func invitationRevokedAt(t *testing.T, db *postgres.DB, tenantID, address string) sql.NullTime {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	var at sql.NullTime
	if err := tx.QueryRowContext(t.Context(),
		`SELECT revoked_at FROM member_invitations WHERE tenant_id = $1 AND email = $2`,
		tenantID, address).Scan(&at); err != nil {
		t.Fatalf("read the invitation to %s: %v", address, err)
	}
	return at
}

// isMember reports whether userID belongs to tenantID.
func isMember(t *testing.T, h *harness, userID, tenantID string) bool {
	t.Helper()
	tenants, err := user.NewPostgresStore(h.db).TenantsForUser(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ut := range tenants {
		if ut.TenantID == tenantID {
			return true
		}
	}
	return false
}

// TestRemoveMember_EndsTheirSessionsInThisWorkspaceOnly: removing a member
// ends their sessions in this workspace, in both modes. Their session in
// another workspace keeps working, and so does that membership.
func TestRemoveMember_EndsTheirSessionsInThisWorkspaceOnly(t *testing.T) {
	h := newHarness(t, "Team Sessions A")
	otherTenant := testutil.CreateTestTenant(t, h.db, "Team Sessions B")
	u := createUser(t, h.db, "two-teams@example.com", h.tenantID, "member")
	attachMember(t, h.db, u.ID, otherTenant)

	testInA := issue(t, h, u.ID, h.tenantID, false)
	liveInA := issue(t, h, u.ID, h.tenantID, true)
	inB := issue(t, h, u.ID, otherTenant, false)

	if err := h.svc.RemoveMember(removalCtx(h), h.tenantID, h.owner, u.ID); err != nil {
		t.Fatalf("remove member: %v", err)
	}

	ctx := context.Background()
	for _, c := range []struct{ name, raw string }{{"test-mode", testInA}, {"live-mode", liveInA}} {
		if _, err := h.sessions.Resolve(ctx, c.raw); !errors.Is(err, session.ErrNotFound) {
			t.Errorf("resolve the %s session in the workspace they left: err = %v, want session.ErrNotFound", c.name, err)
		}
	}
	if _, err := h.sessions.Resolve(ctx, inB); err != nil {
		t.Errorf("resolve the session in the other workspace: %v, want it still active", err)
	}
	if isMember(t, h, u.ID, h.tenantID) {
		t.Error("still a member of the workspace they were removed from")
	}
	if !isMember(t, h, u.ID, otherTenant) {
		t.Error("removal from one workspace also removed the membership in the other")
	}
}

// TestRemoveMember_EndsTheInvitationsTheySent: removing a member revokes the
// invitations they sent that could still be accepted. An expired one is left
// as it is, and another member's invitation keeps working.
func TestRemoveMember_EndsTheInvitationsTheySent(t *testing.T) {
	h := newHarness(t, "Team Invites")
	ctx := removalCtx(h)
	u := createUser(t, h.db, "leaver@example.com", h.tenantID, "member")

	if _, err := h.svc.Invite(ctx, h.tenantID, u.ID, "open@example.com"); err != nil {
		t.Fatalf("invite open: %v", err)
	}
	openToken := h.email.lastToken(t)
	if _, err := h.svc.Invite(ctx, h.tenantID, u.ID, "expired@example.com"); err != nil {
		t.Fatalf("invite expired: %v", err)
	}
	if _, err := testutil.AdminPool(t).ExecContext(ctx,
		`UPDATE member_invitations SET expires_at = now() - interval '1 day' WHERE tenant_id = $1 AND email = $2`,
		h.tenantID, "expired@example.com"); err != nil {
		t.Fatalf("expire the invitation: %v", err)
	}
	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "owners@example.com"); err != nil {
		t.Fatalf("invite from the owner: %v", err)
	}
	ownersToken := h.email.lastToken(t)

	if err := h.svc.RemoveMember(ctx, h.tenantID, h.owner, u.ID); err != nil {
		t.Fatalf("remove member: %v", err)
	}

	if _, err := h.svc.PreviewInvite(ctx, openToken); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("preview the removed member's invitation: err = %v, want ErrValidation", err)
	}
	if _, err := h.svc.AcceptInvite(ctx, openToken, testPassword); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("accept the removed member's invitation: err = %v, want ErrValidation", err)
	}
	if at := invitationRevokedAt(t, h.db, h.tenantID, "open@example.com"); !at.Valid {
		t.Error("the removed member's open invitation has revoked_at NULL, want it set")
	}
	if at := invitationRevokedAt(t, h.db, h.tenantID, "expired@example.com"); at.Valid {
		t.Errorf("the expired invitation got revoked_at %v, want NULL", at.Time)
	}
	if _, err := h.svc.PreviewInvite(ctx, ownersToken); err != nil {
		t.Errorf("preview the owner's invitation: %v, want it still pending", err)
	}
}

// failingAudit refuses every audit row.
type failingAudit struct{}

func (failingAudit) LogInTx(context.Context, *sql.Tx, audit.Entry) error {
	return errors.New("injected: audit insert failed")
}

// failingSessions ends the member's sessions on the removal's transaction
// and then fails, so a test sees whether the sign-out rolls back with it.
type failingSessions struct{ real *session.Service }

func (f failingSessions) EndForMemberTx(ctx context.Context, tx *sql.Tx, userID, tenantID string) error {
	if err := f.real.EndForMemberTx(ctx, tx, userID, tenantID); err != nil {
		return err
	}
	return errors.New("injected: sign-out failed")
}

// TestRemoveMember_AFailedAuditRowLeavesNoChange: when the audit row cannot
// be written, the route answers 500 and nothing changes: the member stays,
// their sessions still work, and their invitation is still open.
func TestRemoveMember_AFailedAuditRowLeavesNoChange(t *testing.T) {
	h := newHarness(t, "Team Audit Fails")
	svc := dashmembers.NewService(dashmembers.NewPostgresStore(h.db), userDirAdapter{user.NewPostgresStore(h.db)},
		h.sessions, failingAudit{}, h.email, staticTenantNamer{name: "Team Audit Fails"}, h.clk, "http://dash.test")
	svc.SetEmailConfigured(true)
	assertRemovalFailsWhole(t, h, svc)
}

// TestRemoveMember_AFailedSignOutLeavesNoChange: when ending the member's
// sessions fails, the route answers 500 and nothing changes, the sessions it
// had already ended included.
func TestRemoveMember_AFailedSignOutLeavesNoChange(t *testing.T) {
	h := newHarness(t, "Team Sign-out Fails")
	svc := dashmembers.NewService(dashmembers.NewPostgresStore(h.db), userDirAdapter{user.NewPostgresStore(h.db)},
		failingSessions{real: h.sessions}, h.audit, h.email, staticTenantNamer{name: "Team Sign-out Fails"}, h.clk, "http://dash.test")
	svc.SetEmailConfigured(true)
	assertRemovalFailsWhole(t, h, svc)
}

// assertRemovalFailsWhole removes a member with two sessions and an open
// invitation through svc's handler, and checks the route answers 500 and
// leaves all of it as it was.
func assertRemovalFailsWhole(t *testing.T, h *harness, svc *dashmembers.Service) {
	t.Helper()
	ctx := removalCtx(h)
	u := createUser(t, h.db, "stays@example.com", h.tenantID, "member")
	testSession := issue(t, h, u.ID, h.tenantID, false)
	liveSession := issue(t, h, u.ID, h.tenantID, true)
	if _, err := h.svc.Invite(ctx, h.tenantID, u.ID, "pending@example.com"); err != nil {
		t.Fatalf("invite: %v", err)
	}
	token := h.email.lastToken(t)

	req := httptest.NewRequest(http.MethodDelete, "/"+u.ID, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	dashmembers.NewHandler(svc, h.sessions, session.CookieConfig{Path: "/"}).Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("DELETE /%s answered %d, want 500; body %s", u.ID, rec.Code, rec.Body.String())
	}

	if !isMember(t, h, u.ID, h.tenantID) {
		t.Error("the failed removal deleted the membership")
	}
	for _, c := range []struct{ name, raw string }{{"test-mode", testSession}, {"live-mode", liveSession}} {
		if _, err := h.sessions.Resolve(context.Background(), c.raw); err != nil {
			t.Errorf("resolve the %s session after the failed removal: %v, want it still active", c.name, err)
		}
	}
	if _, err := h.svc.PreviewInvite(ctx, token); err != nil {
		t.Errorf("preview the member's invitation after the failed removal: %v, want it still pending", err)
	}
	if at := invitationRevokedAt(t, h.db, h.tenantID, "pending@example.com"); at.Valid {
		t.Errorf("the failed removal left revoked_at %v on the invitation, want NULL", at.Time.Format(time.RFC3339))
	}
}
