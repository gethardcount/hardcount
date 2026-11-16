package dashmembers_test

import (
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/dashmembers"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/session"
	"github.com/gethardcount/hardcount/internal/user"
)

// TestInvite_AnExpiredInvitationIsReplacedByANewOne: inviting an address
// whose invitation expired revokes the expired one and creates the new one,
// with one email. The new invitation's audit row names the one it replaced.
func TestInvite_AnExpiredInvitationIsReplacedByANewOne(t *testing.T) {
	h := newHarness(t, "Team Replace")
	ctx := removalCtx(h)

	first, err := h.svc.Invite(ctx, h.tenantID, h.owner, "again@example.com")
	if err != nil {
		t.Fatalf("first invite: %v", err)
	}
	h.clk.Advance(dashmembers.InviteTTL + time.Hour)

	second, err := h.svc.Invite(ctx, h.tenantID, h.owner, "again@example.com")
	if err != nil {
		t.Fatalf("invite the address whose invitation expired: %v, want it created", err)
	}
	if at := invitationRevokedAt(t, h.db, first.ID); !at.Valid {
		t.Error("the expired invitation has revoked_at NULL, want it set")
	}
	if _, err := h.svc.PreviewInvite(ctx, h.email.lastToken(t)); err != nil {
		t.Errorf("preview the new invitation: %v, want it pending", err)
	}
	if n := countInviteEmails(t, h.db, h.tenantID); n != 2 {
		t.Errorf("invite email rows = %d, want 2: one per invitation", n)
	}
	got := rowMetadata(t, h.db, h.tenantID, "member.invited", second.ID)
	want := map[string]any{
		"expires_at":             second.ExpiresAt.Format(time.RFC3339),
		"replaced_invitation_id": first.ID,
	}
	if !maps.Equal(got, want) {
		t.Errorf("new invitation's member.invited metadata = %v, want %v", got, want)
	}
}

// TestInvite_APendingInvitationStillBlocksANewOne: while an invitation can
// still be accepted, a second invite to the address answers the 409 and
// leaves the first one open.
func TestInvite_APendingInvitationStillBlocksANewOne(t *testing.T) {
	h := newHarness(t, "Team Pending")
	ctx := removalCtx(h)

	first, err := h.svc.Invite(ctx, h.tenantID, h.owner, "dup@example.com")
	if err != nil {
		t.Fatalf("first invite: %v", err)
	}
	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "dup@example.com"); !errors.Is(err, errs.ErrAlreadyExists) {
		t.Fatalf("second invite: err = %v, want ErrAlreadyExists", err)
	}
	if at := invitationRevokedAt(t, h.db, first.ID); at.Valid {
		t.Errorf("the pending invitation got revoked_at %v, want NULL", at.Time)
	}
}

// TestInvite_AFailedAuditRowLeavesNothing: when the audit row cannot be
// written, the invite answers 500 and nothing changes: no new invitation, no
// email, and the expired invitation to the address stays open.
func TestInvite_AFailedAuditRowLeavesNothing(t *testing.T) {
	h := newHarness(t, "Team Invite Audit Fails")
	expired, err := h.svc.Invite(removalCtx(h), h.tenantID, h.owner, "expired@example.com")
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	h.clk.Advance(dashmembers.InviteTTL + time.Hour)

	rec := serve(h, failingAuditService(h), http.MethodPost, "/invite", `{"email":"expired@example.com"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("POST /invite answered %d, want 500; body %s", rec.Code, rec.Body.String())
	}

	_, invs, err := h.svc.List(removalCtx(h), h.tenantID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(invs) != 1 || invs[0].ID != expired.ID || invs[0].RevokedAt != nil {
		t.Errorf("invitations after the failed invite = %+v, want only the expired one, not revoked", invs)
	}
	if n := countInviteEmails(t, h.db, h.tenantID); n != 1 {
		t.Errorf("invite email rows = %d, want 1: the first invitation's", n)
	}
}

// TestRevokeInvitation_AFailedAuditRowLeavesItPending: when the audit row
// cannot be written, the revoke answers 500 and the invitation is not
// revoked.
func TestRevokeInvitation_AFailedAuditRowLeavesItPending(t *testing.T) {
	h := newHarness(t, "Team Revoke Audit Fails")
	inv, err := h.svc.Invite(removalCtx(h), h.tenantID, h.owner, "kept@example.com")
	if err != nil {
		t.Fatalf("invite: %v", err)
	}

	rec := serve(h, failingAuditService(h), http.MethodDelete, "/invitations/"+inv.ID, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("DELETE /invitations/%s answered %d, want 500; body %s", inv.ID, rec.Code, rec.Body.String())
	}
	if at := invitationRevokedAt(t, h.db, inv.ID); at.Valid {
		t.Errorf("the failed revoke left revoked_at %v, want NULL", at.Time)
	}
}

// TestAcceptInvite_AFailedAuditRowLeavesNothing: when the member.joined row
// cannot be written, the accept answers 500 and nothing is saved: no account,
// no membership, no session, and the invitation can still be accepted.
func TestAcceptInvite_AFailedAuditRowLeavesNothing(t *testing.T) {
	h := newHarness(t, "Team Accept Audit Fails")
	ctx := removalCtx(h)
	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "joiner@example.com"); err != nil {
		t.Fatalf("invite: %v", err)
	}
	token := h.email.lastToken(t)

	// The public route: no session, so the request carries no mode.
	r := chi.NewRouter()
	dashmembers.NewHandler(failingAuditService(h), session.CookieConfig{Path: "/"}).RegisterPublicRoutes(r)
	req := httptest.NewRequest(http.MethodPost, "/accept-invite",
		strings.NewReader(`{"token":"`+token+`","password":"`+testPassword+`"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("POST /accept-invite answered %d, want 500; body %s", rec.Code, rec.Body.String())
	}

	if _, err := user.NewPostgresStore(h.db).GetByEmail(ctx, "joiner@example.com"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("look up the account after the failed accept: err = %v, want ErrNotFound", err)
	}
	if members, _, err := h.svc.List(ctx, h.tenantID); err != nil || len(members) != 1 {
		t.Errorf("members after the failed accept: got %d (err %v), want only the owner", len(members), err)
	}
	if n := countSessions(t, h.db, h.tenantID); n != 0 {
		t.Errorf("sessions in the workspace after the failed accept: got %d, want 0", n)
	}
	res, err := h.svc.AcceptInvite(ctx, dashmembers.AcceptInput{Token: token, Password: testPassword})
	if err != nil {
		t.Fatalf("accept again with a working audit writer: %v", err)
	}
	if !res.NewAccount || res.RawSessionID == "" {
		t.Errorf("accept result = %+v, want a new account with a session", res)
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
