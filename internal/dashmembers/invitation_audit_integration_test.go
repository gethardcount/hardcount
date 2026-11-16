package dashmembers_test

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/dashmembers"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/session"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/user"
)

// expireInvitation moves invitation id's expires_at a day into the past.
func expireInvitation(t *testing.T, id string) {
	t.Helper()
	if _, err := testutil.AdminPool(t).ExecContext(t.Context(),
		`UPDATE member_invitations SET expires_at = now() - interval '1 day' WHERE id = $1`, id); err != nil {
		t.Fatalf("expire invitation %s: %v", id, err)
	}
}

// invitationsTo counts tenantID's invitations to address, in any state.
func invitationsTo(t *testing.T, db *postgres.DB, tenantID, address string) int {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(t.Context(),
		`SELECT count(*) FROM member_invitations WHERE tenant_id = $1 AND email = $2`, tenantID, address).Scan(&n); err != nil {
		t.Fatalf("count invitations to %s: %v", address, err)
	}
	return n
}

// auditMetadata returns the metadata of each audit row with action on
// resource (resource type user) in tenantID.
func auditMetadata(t *testing.T, db *postgres.DB, tenantID, action, resourceID string) []map[string]any {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(t.Context(), `
		SELECT metadata FROM audit_log
		WHERE tenant_id = $1 AND action = $2 AND resource_type = 'user' AND resource_id = $3`,
		tenantID, action, resourceID)
	if err != nil {
		t.Fatalf("read the %s rows for %s: %v", action, resourceID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var meta map[string]any
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatalf("decode the %s metadata %s: %v", action, raw, err)
		}
		out = append(out, meta)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
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
	dashmembers.NewHandler(svc, h.sessions, session.CookieConfig{Path: "/"}).Routes().ServeHTTP(rec, req)
	return rec
}

// TestInvite_AnExpiredInvitationIsReplacedByANewOne: inviting an address
// whose invitation expired revokes the expired one and creates the new one,
// with one email. The new invitation's audit row names the one it replaced;
// the first invitation's row names none.
func TestInvite_AnExpiredInvitationIsReplacedByANewOne(t *testing.T) {
	h := newHarness(t, "Team Replace")
	ctx := removalCtx(h)

	first, err := h.svc.Invite(ctx, h.tenantID, h.owner, "again@example.com")
	if err != nil {
		t.Fatalf("first invite: %v", err)
	}
	expireInvitation(t, first.ID)

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
	if n := len(h.email.sent); n != 2 || h.email.sent[1].to != "again@example.com" {
		t.Errorf("emails sent = %+v, want the second one to again@example.com", h.email.sent)
	}

	firstRows := auditMetadata(t, h.db, h.tenantID, "member.invited", first.ID)
	wantFirst := map[string]any{"expires_at": first.ExpiresAt.Format(time.RFC3339)}
	if len(firstRows) != 1 || !maps.Equal(firstRows[0], wantFirst) {
		t.Errorf("first invitation's member.invited rows = %v, want one with %v", firstRows, wantFirst)
	}
	secondRows := auditMetadata(t, h.db, h.tenantID, "member.invited", second.ID)
	wantSecond := map[string]any{
		"expires_at":             second.ExpiresAt.Format(time.RFC3339),
		"replaced_invitation_id": first.ID,
	}
	if len(secondRows) != 1 || !maps.Equal(secondRows[0], wantSecond) {
		t.Errorf("new invitation's member.invited rows = %v, want one with %v", secondRows, wantSecond)
	}
}

// TestInvite_APendingInvitationStillBlocksANewOne: while an invitation can
// still be accepted, a second invite to the address answers the 409 and
// writes nothing.
func TestInvite_APendingInvitationStillBlocksANewOne(t *testing.T) {
	h := newHarness(t, "Team Pending")
	ctx := removalCtx(h)

	first, err := h.svc.Invite(ctx, h.tenantID, h.owner, "dup@example.com")
	if err != nil {
		t.Fatalf("first invite: %v", err)
	}
	token := h.email.lastToken(t)

	_, err = h.svc.Invite(ctx, h.tenantID, h.owner, "dup@example.com")
	if !errors.Is(err, errs.ErrAlreadyExists) || err.Error() != "a pending invitation for this email already exists — revoke it first to re-send" {
		t.Fatalf("second invite: err = %v, want the pending-invitation conflict", err)
	}
	if n := invitationsTo(t, h.db, h.tenantID, "dup@example.com"); n != 1 {
		t.Errorf("invitations to the address = %d, want 1", n)
	}
	if n := countInviteEmails(t, h.db, h.tenantID); n != 1 {
		t.Errorf("invite email rows = %d, want 1", n)
	}
	if at := invitationRevokedAt(t, h.db, first.ID); at.Valid {
		t.Errorf("the pending invitation got revoked_at %v, want NULL", at.Time)
	}
	if _, err := h.svc.PreviewInvite(ctx, token); err != nil {
		t.Errorf("preview the first invitation: %v, want it still pending", err)
	}
}

// TestInvite_AFailedAuditRowLeavesNothing: when the audit row cannot be
// written, the invite answers 500 and nothing changes: no invitation, no
// email, and an expired invitation to the address stays open.
func TestInvite_AFailedAuditRowLeavesNothing(t *testing.T) {
	h := newHarness(t, "Team Invite Audit Fails")
	expired, err := h.svc.Invite(removalCtx(h), h.tenantID, h.owner, "expired@example.com")
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	expireInvitation(t, expired.ID)
	svc := failingAuditService(h)

	for _, address := range []string{"fresh@example.com", "expired@example.com"} {
		rec := serve(h, svc, http.MethodPost, "/invite", `{"email":"`+address+`"}`)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("POST /invite for %s answered %d, want 500; body %s", address, rec.Code, rec.Body.String())
		}
	}

	if n := invitationsTo(t, h.db, h.tenantID, "fresh@example.com"); n != 0 {
		t.Errorf("invitations to fresh@example.com = %d, want 0", n)
	}
	if n := invitationsTo(t, h.db, h.tenantID, "expired@example.com"); n != 1 {
		t.Errorf("invitations to expired@example.com = %d, want 1: the expired one only", n)
	}
	if n := countInviteEmails(t, h.db, h.tenantID); n != 1 {
		t.Errorf("invite email rows = %d, want 1: the first invitation's", n)
	}
	if at := invitationRevokedAt(t, h.db, expired.ID); at.Valid {
		t.Errorf("the failed invite left revoked_at %v on the expired invitation, want NULL", at.Time)
	}
}

// TestRevokeInvitation_AFailedAuditRowLeavesItPending: when the audit row
// cannot be written, the revoke answers 500 and the invitation's link still
// works.
func TestRevokeInvitation_AFailedAuditRowLeavesItPending(t *testing.T) {
	h := newHarness(t, "Team Revoke Audit Fails")
	ctx := removalCtx(h)
	inv, err := h.svc.Invite(ctx, h.tenantID, h.owner, "kept@example.com")
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	token := h.email.lastToken(t)

	rec := serve(h, failingAuditService(h), http.MethodDelete, "/invitations/"+inv.ID, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("DELETE /invitations/%s answered %d, want 500; body %s", inv.ID, rec.Code, rec.Body.String())
	}
	if _, err := h.svc.PreviewInvite(ctx, token); err != nil {
		t.Errorf("preview the invitation after the failed revoke: %v, want it still pending", err)
	}
	if at := invitationRevokedAt(t, h.db, inv.ID); at.Valid {
		t.Errorf("the failed revoke left revoked_at %v, want NULL", at.Time)
	}
}
