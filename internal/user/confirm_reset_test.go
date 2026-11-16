package user

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/session"
)

// TestConfirmReset_ADeadLinkIsRefusedBeforeTheHash: a made-up token sent with
// a short password gets the token error. Had ConfirmReset hashed the password
// before it looked at the link, HashPassword would have answered on password.
func TestConfirmReset_ADeadLinkIsRefusedBeforeTheHash(t *testing.T) {
	h := NewHandler(newFakeService(&fakeUserStore{}), session.CookieConfig{Path: "/"}, stubEmailSender{}, "", false) // no live token
	body, _ := json.Marshal(ConfirmResetReq{Token: "made-up-token", Password: "short"})
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/password-reset/confirm", strings.NewReader(string(body))))

	if got := errParam(t, rec); got != "token" {
		t.Errorf("error param = %q, want token", got)
	}
}

// TestConfirmReset_ALinkSpentAfterTheLookIsADeadLink: the link was live when
// ConfirmReset looked at it and was used by another request before the
// transaction spent it. The answer is the same token error.
func TestConfirmReset_ALinkSpentAfterTheLookIsADeadLink(t *testing.T) {
	store := &fakeUserStore{liveLink: true, resetErr: errResetLinkDead}
	h := NewHandler(newFakeService(store), session.CookieConfig{Path: "/"}, stubEmailSender{}, "", false)
	body, _ := json.Marshal(ConfirmResetReq{Token: "a-live-token", Password: "a-valid-looking-password-42"})
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/password-reset/confirm", strings.NewReader(string(body))))

	if got := errParam(t, rec); got != "token" {
		t.Errorf("error param = %q, want token; body %s", got, rec.Body.String())
	}
}
