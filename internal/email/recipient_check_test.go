package email

import (
	"context"
	"slices"
	"testing"
)

// TestSend_RefusesNonBareRecipient: an address stored before the stricter
// check (2026-10-18) is not rewritten, so the sender checks every To and Cc
// address again. One that is not a single bare address fails the send
// before any SMTP traffic, as a permanent error: the outbox marks the row
// failed at once instead of retrying it for three days, and no bounce is
// recorded against the customer.
//
// The first case is the stored form of a quoted CC accepted before: net/mail
// returned it unquoted, and the RCPT command then named x@evil.example.
func TestSend_RefusesNonBareRecipient(t *testing.T) {
	cases := []struct {
		name string
		to   string
		cc   []string
	}{
		{"unquoted CC stored before", "ap@acme.test", []string{"finance@acme.test", "x@evil.example> notify=never@example.com"}},
		{"display-name To", "Bob <bob@acme.test>", nil},
		{"quoted To", `"bob"@acme.test`, nil},
		{"empty To", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newScriptedSMTP(t)
			s := newCCTestSender(t, srv)
			bounces := &recordingBounce{}
			s.SetBounceReporter(bounces)

			err := s.sendRich(context.Background(), richMessage{
				TenantID: "t1", To: c.to, Cc: c.cc,
				Subject: "Invoice INV-1", TextBody: "b", HTMLBody: "<p>b</p>",
			})
			if err == nil || !IsPermanentSendError(err) {
				t.Errorf("sendRich: err %v, want a permanent error", err)
			}
			if rcpts := srv.recordedRcpts(); len(rcpts) != 0 {
				t.Errorf("RCPT sent for %v, want no SMTP traffic", rcpts)
			}
			if got := bounces.addresses(); len(got) != 0 {
				t.Errorf("bounce recorded for %v; a refused address is not a bounce", got)
			}
		})
	}

	t.Run("plain email", func(t *testing.T) {
		srv := newScriptedSMTP(t)
		s := newCCTestSender(t, srv)
		err := s.sendPlain(context.Background(), "t1", "Bob <bob@acme.test>", "", "Reset your password", "b")
		if err == nil || !IsPermanentSendError(err) {
			t.Errorf("sendPlain: err %v, want a permanent error", err)
		}
		if rcpts := srv.recordedRcpts(); len(rcpts) != 0 {
			t.Errorf("RCPT sent for %v, want no SMTP traffic", rcpts)
		}
	})

	t.Run("control: bare addresses are sent", func(t *testing.T) {
		srv := newScriptedSMTP(t)
		s := newCCTestSender(t, srv)
		err := s.sendRich(context.Background(), richMessage{
			TenantID: "t1", To: "ap@acme.test", Cc: []string{"o'neil+ar@acme.co.uk"},
			Subject: "Invoice INV-1", TextBody: "b", HTMLBody: "<p>b</p>",
		})
		if err != nil {
			t.Fatalf("sendRich: %v", err)
		}
		if err := s.sendPlain(context.Background(), "t1", "owner@acme.test", "", "Reset your password", "b"); err != nil {
			t.Fatalf("sendPlain: %v", err)
		}
		want := []string{"ap@acme.test", "o'neil+ar@acme.co.uk", "owner@acme.test"}
		if got := srv.recordedRcpts(); !slices.Equal(got, want) {
			t.Errorf("RCPT set %v, want %v", got, want)
		}
	})
}
