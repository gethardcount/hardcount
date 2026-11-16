package dashmembers_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
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

// invitationRevokedAt reads the revoked_at of invitation id.
func invitationRevokedAt(t *testing.T, db *postgres.DB, id string) sql.NullTime {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	var at sql.NullTime
	if err := tx.QueryRowContext(t.Context(),
		`SELECT revoked_at FROM member_invitations WHERE id = $1`, id).Scan(&at); err != nil {
		t.Fatalf("read invitation %s: %v", id, err)
	}
	return at
}

// rowMetadata reads the metadata of the audit row with action on resource
// type user and resourceID in tenantID.
func rowMetadata(t *testing.T, db *postgres.DB, tenantID, action, resourceID string) map[string]any {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	var raw []byte
	if err := tx.QueryRowContext(t.Context(), `
		SELECT metadata FROM audit_log
		WHERE tenant_id = $1 AND action = $2 AND resource_type = 'user' AND resource_id = $3`,
		tenantID, action, resourceID).Scan(&raw); err != nil {
		t.Fatalf("read the %s row for %s: %v", action, resourceID, err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("decode the %s metadata %s: %v", action, raw, err)
	}
	return meta
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
	if meta := rowMetadata(t, h.db, h.tenantID, "member.removed", u.ID); meta["revoked_invitation_ids"] != nil {
		t.Errorf("member.removed metadata = %v, want no revoked_invitation_ids: the member sent no invitations", meta)
	}
}

// TestRemoveMember_EndsTheInvitationsTheySent: removing a member revokes
// every invitation they sent that is not yet accepted, an expired one
// included, and the audit row lists exactly those. Another member's
// invitation keeps working.
func TestRemoveMember_EndsTheInvitationsTheySent(t *testing.T) {
	h := newHarness(t, "Team Invites")
	ctx := removalCtx(h)
	u := createUser(t, h.db, "leaver@example.com", h.tenantID, "member")

	open, err := h.svc.Invite(ctx, h.tenantID, u.ID, "open@example.com")
	if err != nil {
		t.Fatalf("invite open: %v", err)
	}
	openToken := h.email.lastToken(t)
	expired, err := h.svc.Invite(ctx, h.tenantID, u.ID, "expired@example.com")
	if err != nil {
		t.Fatalf("invite expired: %v", err)
	}
	if _, err := testutil.AdminPool(t).ExecContext(ctx,
		`UPDATE member_invitations SET expires_at = now() - interval '1 day' WHERE id = $1`, expired.ID); err != nil {
		t.Fatalf("expire the invitation: %v", err)
	}
	accepted, err := h.svc.Invite(ctx, h.tenantID, u.ID, "joined@example.com")
	if err != nil {
		t.Fatalf("invite joined: %v", err)
	}
	if _, err := h.svc.AcceptInvite(ctx, dashmembers.AcceptInput{Token: h.email.lastToken(t), Password: testPassword}); err != nil {
		t.Fatalf("accept the invitation before the removal: %v", err)
	}
	owners, err := h.svc.Invite(ctx, h.tenantID, h.owner, "owners@example.com")
	if err != nil {
		t.Fatalf("invite from the owner: %v", err)
	}
	ownersToken := h.email.lastToken(t)

	if err := h.svc.RemoveMember(ctx, h.tenantID, h.owner, u.ID); err != nil {
		t.Fatalf("remove member: %v", err)
	}

	if _, err := h.svc.PreviewInvite(ctx, openToken); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("preview the removed member's invitation: err = %v, want ErrValidation", err)
	}
	if _, err := h.svc.AcceptInvite(ctx, dashmembers.AcceptInput{Token: openToken, Password: testPassword}); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("accept the removed member's invitation: err = %v, want ErrValidation", err)
	}
	for _, inv := range []struct{ name, id string }{{"open", open.ID}, {"expired", expired.ID}} {
		if at := invitationRevokedAt(t, h.db, inv.id); !at.Valid {
			t.Errorf("the removed member's %s invitation has revoked_at NULL, want it set", inv.name)
		}
	}
	if at := invitationRevokedAt(t, h.db, owners.ID); at.Valid {
		t.Errorf("the owner's invitation got revoked_at %v, want NULL", at.Time)
	}
	// An invitation accepted before the removal is history, not authority:
	// it stays accepted and is not listed below.
	if at := invitationRevokedAt(t, h.db, accepted.ID); at.Valid {
		t.Errorf("the removed member's accepted invitation got revoked_at %v, want NULL", at.Time)
	}
	if _, err := h.svc.PreviewInvite(ctx, ownersToken); err != nil {
		t.Errorf("preview the owner's invitation: %v, want it still pending", err)
	}

	listed, _ := rowMetadata(t, h.db, h.tenantID, "member.removed", u.ID)["revoked_invitation_ids"].([]any)
	var got []string
	for _, v := range listed {
		id, _ := v.(string)
		got = append(got, id)
	}
	sort.Strings(got)
	want := []string{open.ID, expired.ID}
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("member.removed revoked_invitation_ids = %v, want %v", got, want)
	}
}

// TestRemoveMember_KeepsTheirInvitationsInOtherWorkspaces: removing a member
// from one workspace leaves the invitations they sent in another workspace
// they belong to.
func TestRemoveMember_KeepsTheirInvitationsInOtherWorkspaces(t *testing.T) {
	h := newHarness(t, "Team Invites A")
	ctx := removalCtx(h)
	otherTenant := testutil.CreateTestTenant(t, h.db, "Team Invites B")
	u := createUser(t, h.db, "in-both@example.com", h.tenantID, "member")
	attachMember(t, h.db, u.ID, otherTenant)

	if _, err := h.svc.Invite(ctx, h.tenantID, u.ID, "to-a@example.com"); err != nil {
		t.Fatalf("invite in the first workspace: %v", err)
	}
	inB, err := h.svc.Invite(ctx, otherTenant, u.ID, "to-b@example.com")
	if err != nil {
		t.Fatalf("invite in the second workspace: %v", err)
	}
	tokenB := h.email.lastToken(t)

	if err := h.svc.RemoveMember(ctx, h.tenantID, h.owner, u.ID); err != nil {
		t.Fatalf("remove member: %v", err)
	}

	if at := invitationRevokedAt(t, h.db, inB.ID); at.Valid {
		t.Errorf("the invitation in the other workspace got revoked_at %v, want NULL", at.Time)
	}
	if _, err := h.svc.PreviewInvite(ctx, tokenB); err != nil {
		t.Errorf("preview the invitation in the other workspace: %v, want it still pending", err)
	}
}

// failingAudit refuses every audit row.
type failingAudit struct{}

func (failingAudit) LogInTx(context.Context, *sql.Tx, audit.Entry) error {
	return errors.New("injected: audit insert failed")
}

// failingAuditService is h's service with an audit writer that refuses every
// row.
func failingAuditService(h *harness) *dashmembers.Service {
	svc := dashmembers.NewService(dashmembers.NewPostgresStore(h.db), userDirAdapter{user.NewPostgresStore(h.db)},
		h.sessions, failingAudit{}, h.email, staticTenantNamer{name: "Team"}, h.clk, "http://dash.test")
	svc.SetEmailConfigured(true)
	return svc
}

// serve sends one request to svc's members routes as the owner's session and
// returns the answer.
func serve(h *harness, svc *dashmembers.Service, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(removalCtx(h))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	dashmembers.NewHandler(svc, session.CookieConfig{Path: "/"}).Routes().ServeHTTP(rec, req)
	return rec
}

// failingSessions ends the member's sessions on the removal's transaction
// and then fails, so a test sees whether the sign-out rolls back with it.
type failingSessions struct{ *session.Service }

func (f failingSessions) EndForMemberTx(ctx context.Context, tx *sql.Tx, userID, tenantID string) error {
	if err := f.Service.EndForMemberTx(ctx, tx, userID, tenantID); err != nil {
		return err
	}
	return errors.New("injected: sign-out failed")
}

// TestRemoveMember_AFailedAuditRowLeavesNoChange: when the audit row cannot
// be written, the route answers 500 and nothing changes: the member stays,
// their sessions still work, and their invitation is still open.
func TestRemoveMember_AFailedAuditRowLeavesNoChange(t *testing.T) {
	h := newHarness(t, "Team Audit Fails")
	assertRemovalFailsWhole(t, h, failingAuditService(h))
}

// TestRemoveMember_AFailedSignOutLeavesNoChange: when ending the member's
// sessions fails, the route answers 500 and nothing changes, the sessions it
// had already ended included.
func TestRemoveMember_AFailedSignOutLeavesNoChange(t *testing.T) {
	h := newHarness(t, "Team Sign-out Fails")
	svc := dashmembers.NewService(dashmembers.NewPostgresStore(h.db), userDirAdapter{user.NewPostgresStore(h.db)},
		failingSessions{h.sessions}, h.audit, h.email, staticTenantNamer{name: "Team Sign-out Fails"}, h.clk, "http://dash.test")
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
	pending, err := h.svc.Invite(ctx, h.tenantID, u.ID, "pending@example.com")
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	token := h.email.lastToken(t)

	rec := serve(h, svc, http.MethodDelete, "/"+u.ID, "")
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
	if at := invitationRevokedAt(t, h.db, pending.ID); at.Valid {
		t.Errorf("the failed removal left revoked_at %v on the invitation, want NULL", at.Time.Format(time.RFC3339))
	}
}
