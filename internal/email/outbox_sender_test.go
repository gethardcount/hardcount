package email

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/payment"
)

// TestOutboxSender_SatisfiesAllInterfaces is a compile-time guarantee that
// OutboxSender is a drop-in replacement for *Sender across every domain that
// consumes email. If a new method is added to any of these interfaces without
// a matching OutboxSender method, this assertion fails to compile — which is
// exactly the signal we want, since a missed method would silently bypass the
// outbox for one email type.
func TestOutboxSender_SatisfiesAllInterfaces(t *testing.T) {
	t.Parallel()
	var s *OutboxSender
	var (
		_ invoice.EmailSender        = s
		_ dunning.EmailNotifier      = s
		_ payment.EmailReceipt       = s
		_ payment.EmailPaymentFailed = s
	)
}

// TestOutboxSender_RequiresTenantID guards the contract: every enqueue must
// carry a tenant_id (enforced by the RLS policy on email_outbox and by
// outbox.Enqueue). A caller that passes "" should error rather than silently
// writing orphan rows — that would break operator visibility.
func TestOutboxSender_RequiresTenantID(t *testing.T) {
	t.Parallel()
	// A nil store is fine — the tenantID check short-circuits before any DB
	// access, so we never reach the store call.
	s := NewOutboxSender(nil)

	err := s.SendInvoice(context.Background(), "", "to@x.com", nil, "n", "inv", 1, "USD", nil, "")
	if err == nil {
		t.Error("SendInvoice with empty tenantID should error")
	}
}

// TestOutboxSender_SuppressedRecipient verifies that when the
// suppression checker reports a recipient is bounced/complained, the
// enqueue is soft-skipped with ErrRecipientSuppressed and no DB write
// is attempted. Nil store proves no write happened (would panic).
func TestOutboxSender_SuppressedRecipient(t *testing.T) {
	t.Parallel()
	s := NewOutboxSender(nil)
	s.SetSuppressionChecker(suppressionFake{suppressed: true, reason: "bounced: 550 user unknown"})

	err := s.SendInvoice(context.Background(), "tnt_a", "bounced@x.com", nil, "n", "inv", 1, "USD", nil, "")
	if !errors.Is(err, ErrRecipientSuppressed) {
		t.Errorf("expected ErrRecipientSuppressed; got %v", err)
	}
}

// TestOutboxSender_SuppressionCheckerErrorFailsOpen — a flaky lookup
// must NOT block legitimate sends. The enqueue should proceed and we
// see the store call (nil store → expected nil-deref panic, which
// confirms we passed the gate). Caught + recovered.
func TestOutboxSender_SuppressionCheckerErrorFailsOpen(t *testing.T) {
	t.Parallel()
	defer func() {
		// Recovery confirms we reached the store call (intended panic)
		// rather than short-circuiting on the suppression error.
		_ = recover()
	}()
	s := NewOutboxSender(nil) // nil store will panic on enqueue
	s.SetSuppressionChecker(suppressionFake{err: errors.New("db down")})

	_ = s.SendInvoice(context.Background(), "tnt_a", "ok@x.com", nil, "n", "inv", 1, "USD", nil, "")
	t.Error("expected the nil-store panic confirming fail-open path")
}

type suppressionFake struct {
	suppressed bool
	reason     string
	err        error
}

func (f suppressionFake) IsSuppressed(_ context.Context, _, _ string) (bool, string, error) {
	return f.suppressed, f.reason, f.err
}

// TestOutboxMessage_EveryPayloadKeyIsClassified makes adding a field to
// outboxMessage a decision about storage: the new key is either removed
// when the email is sent (scrubbedPayloadKeys) or listed here as safe to
// keep on the sent row. A new link or token field that nobody classified
// would otherwise stay in the database for good.
func TestOutboxMessage_EveryPayloadKeyIsClassified(t *testing.T) {
	t.Parallel()
	keptAfterSend := map[string]bool{
		"to": true, "cc": true, "customer_name": true, "invoice_number": true,
		"credit_note_number": true, "amount_cents": true, "currency": true,
		"trigger": true, "attempt_number": true, "max_attempts": true,
		"next_retry_date": true, "reason": true, "failure_reason": true,
		"escalation_outcome": true, "operator_note": true, "inviter_email": true,
		"tenant_name": true, "invoice_written_off": true,
		// A yes/no on which warning copy was sent (#966): no link, no token.
		"card_needs_replacing": true,
	}
	scrubbed := map[string]bool{}
	for _, k := range scrubbedPayloadKeys {
		scrubbed[k] = true
	}
	for _, want := range []string{"setup_url", "update_url", "password_reset_url", "invite_url", "public_token", "pdf"} {
		if !scrubbed[want] {
			t.Errorf("%q must be removed from sent rows", want)
		}
	}

	typ := reflect.TypeOf(outboxMessage{})
	for i := 0; i < typ.NumField(); i++ {
		key, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if keptAfterSend[key] == scrubbed[key] {
			t.Errorf("payload key %q (field %s) must be in exactly one of scrubbedPayloadKeys or keptAfterSend",
				key, typ.Field(i).Name)
		}
	}
	// Keys the readers of sent rows depend on; scrubbing one breaks the
	// invoice timeline, the Sent-emails panel or the Postmark webhook.
	for _, k := range []string{"to", "invoice_number", "attempt_number", "trigger"} {
		if scrubbed[k] {
			t.Errorf("%q is read from sent rows and must not be scrubbed", k)
		}
	}
}
