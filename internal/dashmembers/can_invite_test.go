package dashmembers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/errs"
)

// TestCanInviteAgreesWithInvite: the Team tab turns the invite form off when
// CanInvite is false, so it must be false exactly when Invite refuses on
// server settings, and true when Invite gets past those checks. Both refusals
// happen before the store is touched, so a nil store is enough here.
func TestCanInviteAgreesWithInvite(t *testing.T) {
	cases := []struct {
		name      string
		baseURL   string
		email     bool
		wantCan   bool
		wantCode  string
		wantInMsg string
	}{
		{"no dashboard address", "", true, false, "", "Invites are off"},
		{"no email", "https://app.example.com", false, false, errs.CodeEmailNotConfigured, ""},
		{"neither", "", false, false, "", "Invites are off"},
		{"both set", "https://app.example.com", true, true, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc := NewService(nil, nil, nil, nil, nil, nil, c.baseURL)
			svc.SetEmailConfigured(c.email)
			if got := svc.CanInvite(); got != c.wantCan {
				t.Fatalf("CanInvite() = %v, want %v", got, c.wantCan)
			}
			if c.wantCan {
				return // Invite would go on to the store; the refusals are what this pins.
			}
			_, err := svc.Invite(context.Background(), "t1", "u1", "bob@example.com")
			if !errors.Is(err, errs.ErrInvalidState) {
				t.Fatalf("Invite err = %v, want an invalid-state refusal", err)
			}
			if errs.Code(err) != c.wantCode {
				t.Errorf("code = %q, want %q", errs.Code(err), c.wantCode)
			}
			if c.wantInMsg != "" && !strings.Contains(err.Error(), c.wantInMsg) {
				t.Errorf("message %q does not contain %q", err.Error(), c.wantInMsg)
			}
			// The refusal is shown to whoever clicked Send invite: no setting
			// names on screen.
			if strings.Contains(err.Error(), "DASHBOARD_BASE_URL") || strings.Contains(err.Error(), "SMTP") {
				t.Errorf("message names a server setting: %q", err.Error())
			}
		})
	}
}
