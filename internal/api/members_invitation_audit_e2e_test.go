package api

import (
	"net/http/httptest"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestInviteAndRevoke_OneAuditRowEach: over the real router, an invite and
// the revoke of that invitation each write one audit row, with the signed-in
// owner as actor, the client address, that request's id and the owner's
// session mode.
func TestInviteAndRevoke_OneAuditRowEach(t *testing.T) {
	db := testutil.SetupTestDB(t)
	t.Setenv("HARDCOUNT_BOOTSTRAP_TOKEN", "invitation-audit-bootstrap")
	t.Setenv("DASHBOARD_BASE_URL", "http://dash.invitation-audit.test")
	// Invites are refused on a server with no SMTP. No dispatcher runs in
	// this test, so nothing connects to this host.
	t.Setenv("SMTP_HOST", "smtp.invitation-audit.invalid")
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	const ownerEmail, ownerPassword = "owner@invitation-audit.test", "invitation-audit-owner-pw"
	tenantID := flowStr(flowBootstrap(t, ts, "invitation-audit-bootstrap", ownerEmail, ownerPassword), "tenant", "id")
	ownerCookie := login(t, ts, ownerEmail, ownerPassword)
	ownerID := flowStr(flowDo(t, ts, "GET", "/v1/whoami", flowReq{cookie: ownerCookie}).want(t, 200, "owner whoami").body, "user_id")
	flowDo(t, ts, "POST", "/v1/auth/mode", flowReq{cookie: ownerCookie, body: map[string]any{"livemode": true}}).want(t, 200, "switch the owner to live")

	inviteResp := postWithCookie(t, ts, "/v1/members/invite", ownerCookie, map[string]any{"email": "sam@invitation-audit.test"})
	assertStatus(t, inviteResp, 201)
	inviteRequestID := inviteResp.Header.Get("Hardcount-Request-Id")
	invitationID, _ := readJSON(t, inviteResp)["id"].(string)
	_ = inviteResp.Body.Close()
	if inviteRequestID == "" || invitationID == "" {
		t.Fatalf("invite answer: request id %q, invitation id %q, want both", inviteRequestID, invitationID)
	}

	revokeResp := deleteWithCookie(t, ts, "/v1/members/invitations/"+invitationID, ownerCookie)
	assertStatus(t, revokeResp, 204)
	_ = revokeResp.Body.Close()
	revokeRequestID := revokeResp.Header.Get("Hardcount-Request-Id")
	if revokeRequestID == "" {
		t.Fatal("the revoke answer has no Hardcount-Request-Id")
	}

	type row struct {
		action, actorType, actorID, tenantID string
		livemode                             bool
		ip, requestID                        string
	}
	var rows []row
	func() {
		tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
		if err != nil {
			t.Fatal(err)
		}
		defer postgres.Rollback(tx)
		rs, err := tx.QueryContext(t.Context(), `
			SELECT action, actor_type, actor_id, tenant_id, livemode, coalesce(ip_address, ''), coalesce(request_id, '')
			FROM audit_log
			WHERE action IN ('member.invited', 'member.invite_revoked') AND resource_type = 'user' AND resource_id = $1
			ORDER BY created_at, id`, invitationID)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rs.Close() }()
		for rs.Next() {
			var r row
			if err := rs.Scan(&r.action, &r.actorType, &r.actorID, &r.tenantID, &r.livemode, &r.ip, &r.requestID); err != nil {
				t.Fatal(err)
			}
			rows = append(rows, r)
		}
		if err := rs.Err(); err != nil {
			t.Fatal(err)
		}
	}()

	want := []row{
		{"member.invited", "user", ownerID, tenantID, true, "127.0.0.1", inviteRequestID},
		{"member.invite_revoked", "user", ownerID, tenantID, true, "127.0.0.1", revokeRequestID},
	}
	if len(rows) != len(want) || rows[0] != want[0] || rows[1] != want[1] {
		t.Errorf("audit rows for %s =\n%+v\nwant\n%+v", invitationID, rows, want)
	}
}
