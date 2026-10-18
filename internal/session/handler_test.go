package session

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestCookieFlagsPerEnvironment: staging and production mark the session
// cookie Secure when it is set and when it is cleared, so a browser sends it
// only over HTTPS, or to localhost (ADR-095 Phase 0(b)). Local never does,
// so the dashboard works at http://localhost in every browser, Safari
// included. The other flags are the same everywhere.
func TestCookieFlagsPerEnvironment(t *testing.T) {
	for _, tc := range []struct {
		appEnv string
		secure bool
	}{
		{"", false}, // unset means local
		{"local", false},
		{"staging", true},
		{" Staging ", true},
		{"production", true},
		{"PRODUCTION", true},
	} {
		t.Run("APP_ENV="+tc.appEnv, func(t *testing.T) {
			t.Setenv("APP_ENV", tc.appEnv)
			t.Setenv("VELOX_COOKIE_DOMAIN", "")
			c := DefaultCookieConfig()

			for name, set := range map[string]func(http.ResponseWriter){
				"set":   func(w http.ResponseWriter) { c.SetCookie(w, "raw", time.Now().Add(time.Hour)) },
				"clear": func(w http.ResponseWriter) { c.ClearCookie(w) },
			} {
				w := httptest.NewRecorder()
				set(w)
				got := w.Result().Cookies()
				if len(got) != 1 {
					t.Fatalf("%s: cookies = %v, want one", name, got)
				}
				k := got[0]
				if k.Secure != tc.secure {
					t.Errorf("%s: Secure = %v, want %v", name, k.Secure, tc.secure)
				}
				if k.Name != CookieName || !k.HttpOnly || k.SameSite != http.SameSiteLaxMode || k.Path != "/" || k.Domain != "" {
					t.Errorf("%s: cookie = %+v, want %s, HttpOnly, SameSite=Lax, Path=/, no Domain", name, k, CookieName)
				}
			}
		})
	}
}
