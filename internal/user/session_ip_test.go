package user

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	mw "github.com/sagarsuperuser/velox/internal/api/middleware"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/session"
)

// TestLogin_SessionIPIsTheTrustedClientIP: the session row a login writes
// records the address TrustedRealIP resolved, the same one the audit row and
// the rate limiter use. A client can put anything in X-Forwarded-For, so the
// header only counts when a trusted proxy sent it, and then only the entry
// that proxy added. Until 2026-10-18 the session row took the left-most entry
// of the raw header, so every case below that sends one stored 6.6.6.6.
func TestLogin_SessionIPIsTheTrustedClientIP(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("a-good-password-123"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		trustProxy string // the TRUST_PROXY value
		remoteAddr string // the TCP peer
		xff        string // X-Forwarded-For as it reaches Velox
		want       string
	}{
		{"no proxy trusted: a forged header is ignored", "", "203.0.113.9:5555", "6.6.6.6", "203.0.113.9"},
		{"untrusted peer: a forged header is ignored", "10.0.0.0/8", "203.0.113.9:5555", "6.6.6.6", "203.0.113.9"},
		{"trusted proxy: the client's forged entry is ignored", "10.0.0.0/8", "10.0.0.2:443", "6.6.6.6, 198.51.100.7", "198.51.100.7"},
		{"no header: the peer without its port", "", "203.0.113.9:5555", "", "203.0.113.9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := domain.User{ID: "usr_1", Email: "op@acme.com", PasswordHash: string(hash)}
			store := &fakeUserStore{loginUser: &u, tenants: []domain.UserTenant{{UserID: "usr_1", TenantID: "ten_acme"}}}
			sessions := &recordingSessionStore{}
			h := NewHandler(NewService(store, clock.Real()), session.NewService(sessions),
				session.DefaultCookieConfig(), stubEmailSender{}, "", false)
			rec := &recordingAuditRecorder{}
			h.SetAuditLogger(rec)
			// The router mounts TrustedRealIP globally, ahead of /v1/auth.
			srv := mw.TrustedRealIP(mw.ParseTrustedProxies(c.trustProxy))(h.Routes())

			body, _ := json.Marshal(loginReq{Email: "op@acme.com", Password: "a-good-password-123"})
			req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(string(body)))
			req.RemoteAddr = c.remoteAddr
			if c.xff != "" {
				req.Header.Set("X-Forwarded-For", c.xff)
			}
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("login status = %d, want 200: %s", rr.Code, rr.Body.String())
			}
			if len(sessions.inserted) != 1 {
				t.Fatalf("session rows written = %d, want 1", len(sessions.inserted))
			}
			if got := sessions.inserted[0].IP; got != c.want {
				t.Errorf("session row IP = %q, want %q", got, c.want)
			}
			if len(rec.calls) != 1 {
				t.Fatalf("audit calls = %d, want 1", len(rec.calls))
			}
			if got := rec.calls[0].ip; got != sessions.inserted[0].IP {
				t.Errorf("audit row IP = %q but session row IP = %q; both must record the same address", got, sessions.inserted[0].IP)
			}
		})
	}
}
