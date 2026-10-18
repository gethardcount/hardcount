package dashmembers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	mw "github.com/sagarsuperuser/velox/internal/api/middleware"
	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/dashmembers"
	"github.com/sagarsuperuser/velox/internal/session"
)

// ipAuditRecorder keeps the client IP each audit write carried.
type ipAuditRecorder struct{ ips []string }

func (r *ipAuditRecorder) Log(ctx context.Context, _, _, _, _, _ string, _ map[string]any) error {
	r.ips = append(r.ips, audit.ClientIP(ctx))
	return nil
}

// TestAcceptInvite_SessionIPIsTheTrustedClientIP: accepting an invite as a
// new account signs the invitee in, and the session row records the address
// TrustedRealIP resolved, the same one as the member.joined audit row. The
// client sends a forged X-Forwarded-For entry through a trusted proxy, which
// appends the real address. Until 2026-10-18 the session row stored the
// forged left-most entry, 6.6.6.6.
func TestAcceptInvite_SessionIPIsTheTrustedClientIP(t *testing.T) {
	h := newHarness(t, "Team Forwarded")
	ctx := context.Background()

	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "forwarded@example.com"); err != nil {
		t.Fatalf("invite: %v", err)
	}
	token := h.email.lastToken(t)

	handler := dashmembers.NewHandler(h.svc, h.sessions, session.CookieConfig{Path: "/"})
	rec := &ipAuditRecorder{}
	handler.SetAuditLogger(rec)
	r := chi.NewRouter()
	// The router mounts TrustedRealIP globally, ahead of /v1/auth.
	r.Use(mw.TrustedRealIP(mw.ParseTrustedProxies("10.0.0.0/8")))
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
	if len(rec.ips) != 1 || rec.ips[0] != sess.IP {
		t.Errorf("audit IPs = %q, want exactly one equal to the session row IP %q", rec.ips, sess.IP)
	}
}
