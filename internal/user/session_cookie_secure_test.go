package user

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/session"
)

// TestLogin_SessionCookieSecurePerEnvironment: in staging and production the
// cookie a sign-in sets is always Secure, so a browser sends it only over
// HTTPS, or to localhost (ADR-095 Phase 0(b)). The page the browser is on
// does not change that. Until 2026-10-18 a browser on plain http at a
// localhost address got a cookie without Secure, so Safari could sign in to
// the quickstart; the quickstart now runs with APP_ENV=local instead. In
// local the cookie is never Secure, so http://localhost works in every
// browser.
func TestLogin_SessionCookieSecurePerEnvironment(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("a-good-password-123"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}

	envs := []struct {
		appEnv string
		secure bool
	}{
		{"", false}, // unset means local
		{"local", false},
		{"staging", true},
		{"production", true},
		{" Production ", true},
	}
	pages := []struct {
		name, origin, referer string
	}{
		{"https site", "https://billing.example.com", ""},
		{"plain http public address", "http://billing.example.com", ""},
		{"plain http localhost", "http://localhost:8080", ""},
		{"plain http 127.0.0.1", "http://127.0.0.1:8080", ""},
		{"plain http [::1]", "http://[::1]:8080", ""},
		{"plain http localhost, Referer only", "", "http://localhost:8080/login"},
		{"no Origin or Referer", "", ""},
	}

	for _, e := range envs {
		for _, p := range pages {
			t.Run("APP_ENV="+e.appEnv+"/"+p.name, func(t *testing.T) {
				t.Setenv("APP_ENV", e.appEnv)
				u := domain.User{ID: "usr_1", Email: "op@acme.com", PasswordHash: string(hash)}
				store := &fakeUserStore{loginUser: &u, tenants: []domain.UserTenant{{UserID: "usr_1", TenantID: "ten_acme"}}}
				h := NewHandler(NewService(store, clock.Real()), session.NewService(&recordingSessionStore{}),
					session.DefaultCookieConfig(), stubEmailSender{}, "", false)

				body, _ := json.Marshal(loginReq{Email: "op@acme.com", Password: "a-good-password-123"})
				req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(string(body)))
				if p.origin != "" {
					req.Header.Set("Origin", p.origin)
				}
				if p.referer != "" {
					req.Header.Set("Referer", p.referer)
				}
				rr := httptest.NewRecorder()
				h.Routes().ServeHTTP(rr, req)

				if rr.Code != http.StatusOK {
					t.Fatalf("login status = %d, want 200: %s", rr.Code, rr.Body.String())
				}
				var c *http.Cookie
				for _, got := range rr.Result().Cookies() {
					if got.Name == session.CookieName {
						c = got
					}
				}
				if c == nil {
					t.Fatalf("no %s cookie in %v", session.CookieName, rr.Header().Values("Set-Cookie"))
				}
				if c.Secure != e.secure {
					t.Errorf("Secure = %v, want %v (Set-Cookie: %v)", c.Secure, e.secure, rr.Header().Values("Set-Cookie"))
				}
				// The other flags do not depend on the environment.
				if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Value == "" {
					t.Errorf("cookie = %+v, want HttpOnly, SameSite=Lax, Path=/ and a value", c)
				}
			})
		}
	}
}
