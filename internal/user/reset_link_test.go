package user

import "testing"

// TestBuildResetLink: the reset link is built only from DASHBOARD_BASE_URL,
// never from the request, so a poisoned Host header cannot send the token to
// another domain. With the setting unset, no link is built.
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
