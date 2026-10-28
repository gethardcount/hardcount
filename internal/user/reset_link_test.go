package user

import "testing"

// TestBuildResetLink covers the medium-severity [security] audit finding:
// the password-reset link origin was derived from the request Host header,
// so a poisoned Host would email the victim a link pointing at an attacker
// domain, leaking the single-use reset token. The fix removes the request
// parameter entirely — the link can only come from the configured
// DASHBOARD_BASE_URL — so header poisoning is structurally impossible.
func TestBuildResetLink(t *testing.T) {
	t.Run("uses configured base url", func(t *testing.T) {
		h := &Handler{dashboardBaseURL: "https://app.hardcount.dev"}
		link, ok := h.buildResetLink("tok_abc")
		if !ok {
			t.Fatal("expected ok=true when base url is set")
		}
		if link != "https://app.hardcount.dev/reset-password?token=tok_abc" {
			t.Errorf("unexpected link: %q", link)
		}
	})

	t.Run("fails safe when base url unset", func(t *testing.T) {
		h := &Handler{dashboardBaseURL: ""}
		link, ok := h.buildResetLink("tok_abc")
		if ok {
			t.Error("expected ok=false when base url is unset (no header fallback)")
		}
		if link != "" {
			t.Errorf("expected empty link, got %q", link)
		}
	})
}
