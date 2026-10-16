package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/email"
)

type fakeEmailSender struct {
	configured bool
	sendErr    error
	sentTo     []string
}

func (f *fakeEmailSender) IsConfigured() bool  { return f.configured }
func (f *fakeEmailSender) SMTPHost() string    { return "smtp.example.com" }
func (f *fakeEmailSender) FromAddress() string { return "billing@example.com" }
func (f *fakeEmailSender) TLSMode() string     { return "starttls" }
func (f *fakeEmailSender) SendTestEmail(_ context.Context, _, to string) error {
	f.sentTo = append(f.sentTo, to)
	return f.sendErr
}

type fakeAudit struct{ actions []string }

func (f *fakeAudit) Log(_ context.Context, _, action, resourceType, resourceID, _ string, md map[string]any) error {
	f.actions = append(f.actions, action+":"+resourceType+":"+resourceID+":"+md["action"].(string))
	return nil
}

func newEmailSettingsTestHandler(s *fakeEmailSender, a *fakeAudit) http.Handler {
	env := map[string]string{"HOSTED_INVOICE_BASE_URL": "https://pay.example.com"}
	h := &emailSettingsHandler{
		sender: s,
		links:  emailLinkEnvs(func(k string) string { return env[k] }),
		userEmail: func(_ context.Context, userID string) (string, error) {
			return userID + "@team.example.com", nil
		},
		audit: a,
	}
	return h.Routes()
}

func sessionReq(method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	ctx := auth.WithTenantID(r.Context(), "vlx_ten_1")
	ctx = auth.WithUserID(ctx, "ops")
	ctx = auth.WithKeyType(ctx, auth.KeyTypeSession)
	return r.WithContext(ctx)
}

// The status lists every link env var, set or not, so the page can name the
// missing ones.
func TestEmailSettings_Status(t *testing.T) {
	rr := httptest.NewRecorder()
	newEmailSettingsTestHandler(&fakeEmailSender{configured: true}, &fakeAudit{}).ServeHTTP(rr, sessionReq("GET", "/"))
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var got emailSettingsStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.SMTPConfigured || got.SMTPHost != "smtp.example.com" || got.FromAddress != "billing@example.com" {
		t.Errorf("smtp fields: %+v", got)
	}
	want := map[string]bool{"HOSTED_INVOICE_BASE_URL": true, "PAYMENT_UPDATE_URL": false, "DASHBOARD_BASE_URL": false}
	if len(got.Links) != len(want) {
		t.Fatalf("links: got %+v", got.Links)
	}
	for _, l := range got.Links {
		if set, ok := want[l.Env]; !ok || set != l.Set || l.Effect == "" {
			t.Errorf("link %+v: want set=%v and an effect", l, set)
		}
	}
}

// The test email goes only to the signed-in member, and the response says
// what the SMTP server answered.
func TestEmailSettings_SendTest(t *testing.T) {
	t.Run("sends to the member and audits", func(t *testing.T) {
		s, a := &fakeEmailSender{configured: true}, &fakeAudit{}
		rr := httptest.NewRecorder()
		newEmailSettingsTestHandler(s, a).ServeHTTP(rr, sessionReq("POST", "/test"))
		if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"sent_to":"ops@team.example.com"`) {
			t.Fatalf("got %d %s", rr.Code, rr.Body.String())
		}
		if len(s.sentTo) != 1 || s.sentTo[0] != "ops@team.example.com" {
			t.Errorf("sent to %v, want the member's own address", s.sentTo)
		}
		if len(a.actions) != 1 || a.actions[0] != "send:setting:smtp:test_email_sent" {
			t.Errorf("audit: %v", a.actions)
		}
	})
	t.Run("no SMTP is 409 email_not_configured and sends nothing", func(t *testing.T) {
		s, a := &fakeEmailSender{configured: false}, &fakeAudit{}
		rr := httptest.NewRecorder()
		newEmailSettingsTestHandler(s, a).ServeHTTP(rr, sessionReq("POST", "/test"))
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "email_not_configured") {
			t.Fatalf("got %d %s", rr.Code, rr.Body.String())
		}
		if len(s.sentTo) != 0 || len(a.actions) != 0 {
			t.Errorf("sent %v audited %v; want neither", s.sentTo, a.actions)
		}
	})
	t.Run("an SMTP refusal is 502 smtp_error with the server's answer, not audited", func(t *testing.T) {
		s, a := &fakeEmailSender{configured: true, sendErr: errors.New("smtp auth: 535 5.7.8 bad credentials")}, &fakeAudit{}
		rr := httptest.NewRecorder()
		newEmailSettingsTestHandler(s, a).ServeHTTP(rr, sessionReq("POST", "/test"))
		if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "smtp_error") || !strings.Contains(rr.Body.String(), "535") {
			t.Fatalf("got %d %s", rr.Code, rr.Body.String())
		}
		if len(a.actions) != 0 {
			t.Errorf("audited a failed send: %v", a.actions)
		}
	})
	t.Run("ErrSMTPNotConfigured from the sender is 409", func(t *testing.T) {
		s := &fakeEmailSender{configured: true, sendErr: email.ErrSMTPNotConfigured}
		rr := httptest.NewRecorder()
		newEmailSettingsTestHandler(s, &fakeAudit{}).ServeHTTP(rr, sessionReq("POST", "/test"))
		if rr.Code != http.StatusConflict {
			t.Fatalf("got %d %s", rr.Code, rr.Body.String())
		}
	})
	t.Run("an API key can't send a test email", func(t *testing.T) {
		s := &fakeEmailSender{configured: true}
		r := httptest.NewRequest("POST", "/test", nil)
		r = r.WithContext(auth.WithKeyType(auth.WithTenantID(r.Context(), "vlx_ten_1"), auth.KeyTypeSecret))
		rr := httptest.NewRecorder()
		newEmailSettingsTestHandler(s, &fakeAudit{}).ServeHTTP(rr, r)
		if rr.Code != http.StatusForbidden || len(s.sentTo) != 0 {
			t.Fatalf("got %d, sent %v", rr.Code, s.sentTo)
		}
	})
}
