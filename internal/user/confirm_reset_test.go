package user

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/session"
)

// TestConfirmReset_ADeadLinkIsRefusedBeforeTheHash: a made-up token gets the
// token error before any password work. ConfirmReset looks the link up
// first, then hashes, then calls ResetPassword, so a store that sees no
// ResetPassword call means the hash was not reached. The short password
// proves it a second way: HashPassword would have answered on password.
func TestConfirmReset_ADeadLinkIsRefusedBeforeTheHash(t *testing.T) {
	for _, password := range []string{"a-valid-looking-password-42", "short"} {
		store := &fakeUserStore{} // LookupResetToken finds no live token
		h := NewHandler(newFakeService(store), session.NewService(&recordingSessionStore{}),
			session.CookieConfig{Path: "/"}, stubEmailSender{}, "", false)
		body, _ := json.Marshal(ConfirmResetReq{Token: "made-up-token", Password: password})
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/password-reset/confirm", strings.NewReader(string(body))))

		if got := errParam(t, rec); got != "token" {
			t.Errorf("password %q: error param = %q, want token", password, got)
		}
		if store.resetCalls != 0 {
			t.Errorf("password %q: ResetPassword called %d times, want 0", password, store.resetCalls)
		}
	}
}

// TestConfirmReset_ALinkSpentAfterTheLookIsADeadLink: the link was live when
// ConfirmReset looked at it and was used by another request before the
// transaction spent it. The answer is the same token error.
func TestConfirmReset_ALinkSpentAfterTheLookIsADeadLink(t *testing.T) {
	store := &fakeUserStore{liveLink: true, resetErr: errResetLinkDead}
	h := NewHandler(newFakeService(store), session.NewService(&recordingSessionStore{}),
		session.CookieConfig{Path: "/"}, stubEmailSender{}, "", false)
	body, _ := json.Marshal(ConfirmResetReq{Token: "a-live-token", Password: "a-valid-looking-password-42"})
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/password-reset/confirm", strings.NewReader(string(body))))

	if got := errParam(t, rec); got != "token" {
		t.Errorf("error param = %q, want token; body %s", got, rec.Body.String())
	}
	if store.resetCalls != 1 {
		t.Errorf("ResetPassword called %d times, want 1", store.resetCalls)
	}
}
