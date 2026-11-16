package api

import (
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestRemoveMember_OneAuditRowAndTheMemberSignedOut: over the real router,
// removing a member answers 204, writes one member.removed row with the
// remover as actor, the client address, the request id and the remover's
// session mode, signs the removed member out, and ends the invitation they
// had sent. An invite and its revoke write one row each, attributed the same
// way.
func TestRemoveMember_OneAuditRowAndTheMemberSignedOut(t *testing.T) {
	db := testutil.SetupTestDB(t)
	t.Setenv("HARDCOUNT_BOOTSTRAP_TOKEN", "remove-member-bootstrap")
	t.Setenv("DASHBOARD_BASE_URL", "http://dash.remove-member.test")
	// Invites are refused on a server with no SMTP. No dispatcher runs in
	// this test, so nothing connects to this host.
	t.Setenv("SMTP_HOST", "smtp.remove-member.invalid")
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	const ownerEmail, ownerPassword = "owner@remove-member.test", "remove-member-owner-pw"
	tenantID := flowStr(flowBootstrap(t, ts, "remove-member-bootstrap", ownerEmail, ownerPassword), "tenant", "id")
	ownerCookie := login(t, ts, ownerEmail, ownerPassword)
	ownerID := flowStr(flowDo(t, ts, "GET", "/v1/whoami", flowReq{cookie: ownerCookie}).want(t, 200, "owner whoami").body, "user_id")

	flowDo(t, ts, "POST", "/v1/members/invite", flowReq{cookie: ownerCookie, body: map[string]any{
		"email": "teammate@remove-member.test",
	}}).want(t, 201, "invite the teammate")
	accept := flowDo(t, ts, "POST", "/v1/auth/accept-invite", flowReq{body: map[string]any{
		"token": flowEmailLinkToken(t, db, "member_invite", "invite_url"), "password": "remove-member-mate-pw",
	}}).want(t, 200, "accept the invitation")
	memberID := flowStr(accept.body, "user_id")
	memberCookie := login(t, ts, "teammate@remove-member.test", "remove-member-mate-pw")

	flowDo(t, ts, "POST", "/v1/members/invite", flowReq{cookie: memberCookie, body: map[string]any{
		"email": "friend@remove-member.test",
	}}).want(t, 201, "the teammate invites a friend")
	friendToken := flowEmailLinkToken(t, db, "member_invite", "invite_url")

	flowDo(t, ts, "POST", "/v1/auth/mode", flowReq{cookie: ownerCookie, body: map[string]any{"livemode": true}}).want(t, 200, "switch the owner to live")
	invite := postWithCookie(t, ts, "/v1/members/invite", ownerCookie, map[string]any{"email": "sam@remove-member.test"})
	assertStatus(t, invite, 201)
	invitationID, _ := readJSON(t, invite)["id"].(string)
	_ = invite.Body.Close()
	revoke := deleteWithCookie(t, ts, "/v1/members/invitations/"+invitationID, ownerCookie)
	assertStatus(t, revoke, 204)
	_ = revoke.Body.Close()
	remove := deleteWithCookie(t, ts, "/v1/members/"+memberID, ownerCookie)
	assertStatus(t, remove, 204)
	_ = remove.Body.Close()

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
			WHERE action IN ('member.invited', 'member.invite_revoked', 'member.removed')
			  AND resource_type = 'user' AND resource_id IN ($1, $2)
			ORDER BY created_at, id`, invitationID, memberID)
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
		{"member.invited", "user", ownerID, tenantID, true, "127.0.0.1", invite.Header.Get("Hardcount-Request-Id")},
		{"member.invite_revoked", "user", ownerID, tenantID, true, "127.0.0.1", revoke.Header.Get("Hardcount-Request-Id")},
		{"member.removed", "user", ownerID, tenantID, true, "127.0.0.1", remove.Header.Get("Hardcount-Request-Id")},
	}
	if !slices.Equal(rows, want) {
		t.Errorf("audit rows =\n%+v\nwant\n%+v", rows, want)
	}

	flowDo(t, ts, "GET", "/v1/whoami", flowReq{cookie: memberCookie}).want(t, 401, "the removed teammate's whoami")
	flowDo(t, ts, "GET", "/v1/auth/invite/"+friendToken, flowReq{}).want(t, 422, "preview the removed teammate's invitation")
}
