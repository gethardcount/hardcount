package user

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/session"
)

// TestLogin_SessionCookieSecurePerEnvironment: the sign-in sets its cookie
// with the cookie config it was built with. In staging and production the
// cookie is Secure (ADR-095 Phase 0(b)); in local it is not, so
// http://localhost works in every browser.
func TestLogin_SessionCookieSecurePerEnvironment(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("a-good-password-123"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range []struct {
		appEnv string
		secure bool
	}{
		{"local", false},
		{"staging", true},
		{"production", true},
	} {
		t.Run("APP_ENV="+e.appEnv, func(t *testing.T) {
			u := domain.User{ID: "usr_1", Email: "op@acme.com", PasswordHash: string(hash)}
			store := &fakeUserStore{loginUser: &u, tenants: []domain.UserTenant{{UserID: "usr_1", TenantID: "ten_acme"}}}
			h := NewHandler(newFakeService(store), session.DefaultCookieConfig(e.appEnv), stubEmailSender{}, "", false)

			body, _ := json.Marshal(LoginReq{Email: "op@acme.com", Password: "a-good-password-123"})
			rr := httptest.NewRecorder()
			h.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(string(body))))

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
