package invoice

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// spyCustomerReader returns a fixed customer (or an error) and counts lookups,
// so a test can assert the email-presence branch, the read-error fallback, AND
// the lazy-fetch guard (the customer is looked up only for the invoices that
// actually reach the no_payment_method branch, not on every read).
type spyCustomerReader struct {
	cust  domain.Customer
	err   error
	calls int
}

func (s *spyCustomerReader) Get(_ context.Context, _, _ string) (domain.Customer, error) {
	s.calls++
	return s.cust, s.err
}

// readyPMReader is a PaymentMethodReader that reports a ready payment method,
// so a test can drive atc.HasPaymentMethod=true and assert the lazy guard skips
// the customer lookup for has-PM invoices.
type readyPMReader struct{}

func (readyPMReader) GetPaymentSetup(_ context.Context, _, _ string) (domain.CustomerPaymentSetup, error) {
	return domain.CustomerPaymentSetup{SetupStatus: domain.PaymentSetupReady, StripeCustomerID: "cus_stripe_1"}, nil
}

func finalizedUnpaidNoPM() domain.Invoice {
	return domain.Invoice{
		ID:            "inv_1",
		TenantID:      "t1",
		CustomerID:    "cus_1",
		Status:        domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentPending,
	}
}

// TestAttachAttention_NoPaymentMethod_EmailVariant pins the real service
// wiring end to end: attachAttention reads the customer's email presence and
// the classifier renders the honest no_payment_method banner variant — because
// the engine's finalize-time setup-link email skips silently when there's no
// address, so a hardcoded "we emailed a link" lied on exactly those invoices.
// It also pins the lazy-fetch guard: the customer is looked up ONLY for the
// finalized-unpaid-no-PM invoices that reach the branch, so list reads don't
// pay a customer lookup per row.
func TestAttachAttention_NoPaymentMethod_EmailVariant(t *testing.T) {
	ctx := context.Background()

	t.Run("customer has email -> emailed-a-link variant, one lookup", func(t *testing.T) {
		spy := &spyCustomerReader{cust: domain.Customer{Email: "ops@acme.test"}}
		svc := NewService(newMemStore(), nil, newMemNumberer())
		svc.SetCustomerReader(spy)

		inv := svc.attachAttention(ctx, finalizedUnpaidNoPM())
		if inv.Attention == nil || inv.Attention.Reason != domain.AttentionReasonNoPaymentMethod {
			t.Fatalf("expected no_payment_method attention, got %+v", inv.Attention)
		}
		if !strings.Contains(inv.Attention.Message, "\nA setup link is emailed to the customer once") {
			t.Errorf("has-email variant must state the engine's send behavior, got %q", inv.Attention.Message)
		}
		if strings.Contains(inv.Attention.Message, "has been emailed") {
			t.Errorf("banner must not assert a completed send it cannot observe, got %q", inv.Attention.Message)
		}
		var hasResend bool
		for _, a := range inv.Attention.Actions {
			if a.Code == domain.AttentionActionSendReminder {
				hasResend = true
			}
		}
		if !hasResend {
			t.Errorf("has-email variant must offer a resend, got %+v", inv.Attention.Actions)
		}
		if spy.calls != 1 {
			t.Errorf("expected exactly one customer lookup, got %d", spy.calls)
		}
	})

	t.Run("customer has no email -> honest no-address variant, no resend", func(t *testing.T) {
		spy := &spyCustomerReader{cust: domain.Customer{Email: ""}}
		svc := NewService(newMemStore(), nil, newMemNumberer())
		svc.SetCustomerReader(spy)

		inv := svc.attachAttention(ctx, finalizedUnpaidNoPM())
		if inv.Attention == nil {
			t.Fatalf("expected attention")
		}
		assertNoEmailVariant(t, inv.Attention)
	})

	t.Run("customer read errors -> honest undetermined variant (conservative)", func(t *testing.T) {
		// A transient customer-read error leaves CustomerHasEmail false. The
		// banner must NOT flip to the has-email "we emailed a link" claim (the
		// original lie) — it renders the same honest, behavior-stated variant
		// that never asserts this customer's email state, so it's correct even
		// though we couldn't determine it.
		spy := &spyCustomerReader{cust: domain.Customer{Email: "ops@acme.test"}, err: errors.New("db blip")}
		svc := NewService(newMemStore(), nil, newMemNumberer())
		svc.SetCustomerReader(spy)

		inv := svc.attachAttention(ctx, finalizedUnpaidNoPM())
		if spy.calls != 1 {
			t.Fatalf("expected the lookup to be attempted once, got %d", spy.calls)
		}
		assertNoEmailVariant(t, inv.Attention)
	})

	t.Run("email sending not set up -> no-send variant, no resend", func(t *testing.T) {
		// The server flag, not the customer: an address on file is no use when
		// nothing can be sent, and Resend would answer 409.
		spy := &spyCustomerReader{cust: domain.Customer{Email: "ops@acme.test"}}
		svc := NewService(newMemStore(), nil, newMemNumberer())
		svc.SetCustomerReader(spy)
		svc.SetSetupLinkEmailConfigured(false)

		att := svc.attachAttention(ctx, finalizedUnpaidNoPM()).Attention
		if att == nil || !strings.Contains(att.Message, "\nThis server can't email setup links, so none was sent") {
			t.Fatalf("email-off variant missing, got %+v", att)
		}
		for _, a := range att.Actions {
			if a.Code == domain.AttentionActionSendReminder {
				t.Errorf("email-off variant must not offer a resend that is refused, got %+v", att.Actions)
			}
		}

		// Control: the same service with email configured says the link is sent.
		svc.SetSetupLinkEmailConfigured(true)
		att = svc.attachAttention(ctx, finalizedUnpaidNoPM()).Attention
		if att == nil || !strings.Contains(att.Message, "\nA setup link is emailed to the customer once") {
			t.Fatalf("configured control lost the sent variant, got %+v", att)
		}
	})

	t.Run("paid invoice -> no customer lookup (lazy guard: status)", func(t *testing.T) {
		spy := &spyCustomerReader{cust: domain.Customer{Email: "ops@acme.test"}}
		svc := NewService(newMemStore(), nil, newMemNumberer())
		svc.SetCustomerReader(spy)

		inv := finalizedUnpaidNoPM()
		inv.Status = domain.InvoicePaid // terminal: never reaches no_payment_method
		_ = svc.attachAttention(ctx, inv)
		if spy.calls != 0 {
			t.Errorf("a paid invoice must not trigger a customer lookup, got %d", spy.calls)
		}
	})

	t.Run("finalized but not pending -> no customer lookup (lazy guard: payment_status)", func(t *testing.T) {
		spy := &spyCustomerReader{cust: domain.Customer{Email: "ops@acme.test"}}
		svc := NewService(newMemStore(), nil, newMemNumberer())
		svc.SetCustomerReader(spy)

		inv := finalizedUnpaidNoPM()
		inv.PaymentStatus = domain.PaymentProcessing // in-flight, not the no-PM branch
		_ = svc.attachAttention(ctx, inv)
		if spy.calls != 0 {
			t.Errorf("a non-pending invoice must not trigger a customer lookup, got %d", spy.calls)
		}
	})

	t.Run("has payment method -> no customer lookup (lazy guard: HasPaymentMethod)", func(t *testing.T) {
		spy := &spyCustomerReader{cust: domain.Customer{Email: "ops@acme.test"}}
		svc := NewService(newMemStore(), nil, newMemNumberer())
		svc.SetCustomerReader(spy)
		svc.SetPaymentMethodReader(readyPMReader{}) // customer has a ready PM

		_ = svc.attachAttention(ctx, finalizedUnpaidNoPM())
		if spy.calls != 0 {
			t.Errorf("a has-PM invoice must not trigger a customer email lookup, got %d", spy.calls)
		}
	})
}

// assertNoEmailVariant checks the honest no-email/undetermined no_payment_method
// banner: it never claims a link was emailed, states the conditional engine
// behavior instead of this customer's email state, and drops the un-sendable
// resend action.
func assertNoEmailVariant(t *testing.T, att *domain.Attention) {
	t.Helper()
	if att == nil || att.Reason != domain.AttentionReasonNoPaymentMethod {
		t.Fatalf("expected no_payment_method attention, got %+v", att)
	}
	if strings.Contains(att.Message, "has been emailed") {
		t.Errorf("no-email variant must NOT claim a send happened, got %q", att.Message)
	}
	if !strings.Contains(att.Message, "\nSetup links are emailed only to customers with an email address") {
		t.Errorf("no-email variant must state the conditional engine behavior, got %q", att.Message)
	}
	for _, a := range att.Actions {
		if a.Code == domain.AttentionActionSendReminder {
			t.Errorf("no-email variant must NOT offer a resend that can't send, got %+v", att.Actions)
		}
	}
}

// TestAttachAttention_NoPaymentMethod_ReadsPauseForCardlessInvoices pins the
// service half of the paused branch: the pause state used to be read only for
// queued invoices with a card, so a card-less invoice on a paused
// subscription promised "charged automatically once the customer adds a card"
// although the sweep that charges on attach skips paused subscriptions.
func TestAttachAttention_NoPaymentMethod_ReadsPauseForCardlessInvoices(t *testing.T) {
	ctx := context.Background()
	inv := finalizedUnpaidNoPM()
	inv.SubscriptionID = "sub_1"
	inv.AutoChargePending = false // not queued: the old guard skipped the read

	for _, paused := range []bool{true, false} {
		pauses := &stubPauseReader{paused: paused}
		svc := NewService(newMemStore(), nil, newMemNumberer())
		svc.SetCustomerReader(&spyCustomerReader{cust: domain.Customer{Email: "ops@acme.test"}})
		svc.SetSubscriptionPauseReader(pauses)

		att := svc.attachAttention(ctx, inv).Attention
		if att == nil || att.Reason != domain.AttentionReasonNoPaymentMethod {
			t.Fatalf("paused=%v: want no_payment_method, got %+v", paused, att)
		}
		if pauses.calls != 1 {
			t.Errorf("paused=%v: pause state read %d times, want 1", paused, pauses.calls)
		}
		pausedLine := strings.HasPrefix(att.Message, "Collection is paused on the subscription")
		if pausedLine != paused {
			t.Errorf("paused=%v: message = %q", paused, att.Message)
		}
	}

	// A card on file and nothing queued never reaches either branch that
	// reads the pause, so it must not pay for the read.
	pauses := &stubPauseReader{paused: true}
	svc := NewService(newMemStore(), nil, newMemNumberer())
	svc.SetPaymentMethodReader(readyPMReader{})
	svc.SetSubscriptionPauseReader(pauses)
	_ = svc.attachAttention(ctx, inv)
	if pauses.calls != 0 {
		t.Errorf("has-card, unqueued invoice read the pause state %d times, want 0", pauses.calls)
	}
}
