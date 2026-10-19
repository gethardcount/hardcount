package email

import (
	"context"
	"testing"
)

// TestSenderRefusal_IsNotABounce: a relay that refuses the SENDER must not
// mark the customer bounced. SendGrid, the first relay in
// docs/ops/email-setup.md, answers an unverified sender after the message,
// with no status code: "550 The from address does not match a verified
// Sender Identity." (msmtp logs it as "the server did not accept the mail",
// its end-of-message error). Read as a bounce, the email was dead-lettered
// at once and the customer marked bounced, so every later email to them was
// dropped before it was queued and the relay-broken page never fired. It
// must be retried instead. Control: a 550 to RCPT TO, on the same server, is
// still the customer's bounce.
func TestSenderRefusal_IsNotABounce(t *testing.T) {
	srv := newScriptedSMTP(t)
	srv.dataReply = "550 The from address does not match a verified Sender Identity. Mail cannot be sent until this error is resolved."
	srv.rcptReplies["gone@acme.test"] = "550 5.1.1 no such user"
	s := newCCTestSender(t, srv)
	bounces := &recordingBounce{}
	s.SetBounceReporter(bounces)

	err := s.sendRich(context.Background(), richMessage{
		TenantID: "t1", To: "ap@acme.test",
		Subject: "Invoice INV-1", TextBody: "hi", HTMLBody: "<p>hi</p>",
	})
	if err == nil {
		t.Fatal("a refused message must fail the send")
	}
	if addrs := bounces.addresses(); len(addrs) != 0 {
		t.Errorf("the relay refused the sender, and %v was marked bounced: every later email to it is dropped", addrs)
	}
	if IsPermanentSendError(err) {
		t.Errorf("the relay refused the sender, and the email was dead-lettered at once instead of retried: %v", err)
	}

	// Control: the recipient refused at RCPT TO is a bounce.
	err = s.sendRich(context.Background(), richMessage{
		TenantID: "t1", To: "gone@acme.test",
		Subject: "Invoice INV-2", TextBody: "hi", HTMLBody: "<p>hi</p>",
	})
	if err == nil {
		t.Fatal("a refused recipient must fail the send")
	}
	if addrs := bounces.addresses(); len(addrs) != 1 || addrs[0] != "gone@acme.test" {
		t.Errorf("bounces after a refused recipient: got %v, want [gone@acme.test]", addrs)
	}
	if !IsPermanentSendError(err) {
		t.Errorf("a refused recipient must be dead-lettered at once: %v", err)
	}
}
