package dashmembers_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	mw "github.com/gethardcount/hardcount/internal/api/middleware"
	"github.com/gethardcount/hardcount/internal/dashmembers"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/session"
)

// TestAcceptInvite_SessionIPIsTheTrustedClientIP: accepting an invite as a
// new account signs the invitee in, and the session row records the address
// TrustedClientIP resolved, the same one as the member.joined audit row. The
// client sends a forged X-Forwarded-For entry through a trusted proxy, which
// appends the real address.
func TestAcceptInvite_SessionIPIsTheTrustedClientIP(t *testing.T) {
	h := newHarness(t, "Team Forwarded")
	ctx := postgres.WithLivemode(context.Background(), false)

	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "forwarded@example.com"); err != nil {
		t.Fatalf("invite: %v", err)
	}
	token := h.email.lastToken(t)

	handler := dashmembers.NewHandler(h.svc, session.CookieConfig{Path: "/"})
	r := chi.NewRouter()
	// The router mounts TrustedClientIP globally, ahead of /v1/auth.
	r.Use(mw.TrustedClientIP(mw.ParseTrustedProxies("10.0.0.0/8")))
	handler.RegisterPublicRoutes(r)

	body, _ := json.Marshal(map[string]string{"token": token, "password": testPassword})
	req := httptest.NewRequest(http.MethodPost, "/accept-invite", strings.NewReader(string(body)))
	req.RemoteAddr = "10.0.0.2:443"
	req.Header.Set("X-Forwarded-For", "6.6.6.6, 198.51.100.7")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("accept status = %d, want 200: %s", rr.Code, rr.Body.String())
	}

	var raw string
	for _, c := range rr.Result().Cookies() {
		if c.Name == session.CookieName {
			raw = c.Value
		}
	}
	if raw == "" {
		t.Fatal("a new-account accept must set the session cookie")
	}
	sess, err := h.sessions.Resolve(ctx, raw)
	if err != nil {
		t.Fatalf("resolve the minted session: %v", err)
	}
	if sess.IP != "198.51.100.7" {
		t.Errorf("session row IP = %q, want 198.51.100.7 (the address the trusted proxy added)", sess.IP)
	}
	if ips := joinedRowIPs(t, h, sess.UserID); len(ips) != 1 || ips[0] != "198.51.100.7" {
		t.Errorf("member.joined row IPs = %q, want exactly one, 198.51.100.7", ips)
	}
}

// joinedRowIPs reads the ip_address of each member.joined row for userID in
// h's workspace.
func joinedRowIPs(t *testing.T, h *harness, userID string) []string {
	t.Helper()
	tx, err := h.db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(t.Context(), `
		SELECT ip_address FROM audit_log
		WHERE tenant_id = $1 AND action = 'member.joined' AND resource_id = $2`,
		h.tenantID, userID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var ips []string
	for rows.Next() {
		var ip sql.NullString
		if err := rows.Scan(&ip); err != nil {
			t.Fatal(err)
		}
		ips = append(ips, ip.String)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ips
}
