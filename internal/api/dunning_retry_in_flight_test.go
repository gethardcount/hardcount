package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/dunning"
	"github.com/gethardcount/hardcount/internal/payment"
)

// attemptsOnlyStore is the retry adapter's invoice store for chargedOutcome,
// which reads only the attempt rows.
type attemptsOnlyStore struct {
	attempts []domain.InvoiceChargeAttempt
	err      error
}

func (s attemptsOnlyStore) ListChargeAttemptsByInvoice(context.Context, string, string) ([]domain.InvoiceChargeAttempt, error) {
	return s.attempts, s.err
}
func (attemptsOnlyStore) Get(context.Context, string, string) (domain.Invoice, error) {
	panic("not used")
}
func (attemptsOnlyStore) ClaimChargeForDunningRetry(context.Context, string, string) (domain.ChargeHold, error) {
	panic("not used")
}
func (attemptsOnlyStore) ReleaseChargeHold(context.Context, string, string, domain.ChargeHold) error {
	panic("not used")
}
func (attemptsOnlyStore) MarkPaid(context.Context, string, string, string, time.Time) (domain.Invoice, error) {
	panic("not used")
}
func (attemptsOnlyStore) SettleZeroDue(context.Context, string, string, time.Time) (domain.Invoice, bool, error) {
	panic("not used")
}
func (attemptsOnlyStore) RetryBlockingDecline(context.Context, string, string, string) (string, error) {
	panic("not used")
}

// TestChargedOutcome: a dunning retry's charge that returned no error closes
// the run as recovered only when it left the invoice paid (#1289). Before, any
// nil from the charge was "recovered", including a payment still processing.
func TestChargedOutcome(t *testing.T) {
	ctx := context.Background()
	open := domain.Invoice{ID: "inv_1", Status: domain.InvoiceFinalized}
	with := func(status domain.InvoiceStatus, ps domain.InvoicePaymentStatus, pi string) domain.Invoice {
		inv := open
		inv.Status, inv.PaymentStatus, inv.StripePaymentIntentID = status, ps, pi
		return inv
	}
	// The retry's own hold, and its release by token (retryPayment's release).
	hold := domain.ChargeHold{Until: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)}
	releases := 0
	outcome := func(a *paymentRetrierAdapter, inv domain.Invoice) error {
		return a.chargedOutcome(ctx, "t1", inv, hold, func() { releases++ })
	}

	t.Run("paid recovers the run", func(t *testing.T) {
		a := &paymentRetrierAdapter{invoiceStore: attemptsOnlyStore{}}
		if err := outcome(a, with(domain.InvoicePaid, domain.PaymentSucceeded, "pi_ok")); err != nil {
			t.Fatalf("got %v, want nil", err)
		}
	})

	for _, ps := range []domain.InvoicePaymentStatus{domain.PaymentProcessing, domain.PaymentUnknown} {
		t.Run(string(ps)+" waits on its PaymentIntent", func(t *testing.T) {
			a := &paymentRetrierAdapter{invoiceStore: attemptsOnlyStore{}}
			err := outcome(a, with(domain.InvoiceFinalized, ps, "pi_slow"))
			var re *dunning.RetryError
			if !errors.Is(err, dunning.ErrPaymentInFlight) || !errors.As(err, &re) || re.PaymentIntentID != "pi_slow" {
				t.Fatalf("got %#v, want ErrPaymentInFlight naming pi_slow", err)
			}
			if re.Hold != hold || releases != 0 {
				t.Errorf("hold %v released %d times, want the charge's hold carried to dunning and kept", re.Hold, releases)
			}
			if got := classifyDunningRetryResult(ctx, "inv_1", err); got != err {
				t.Errorf("classified as %v, want counted as it is", got)
			}
		})
	}

	t.Run("in flight with no PaymentIntent is a loud skip", func(t *testing.T) {
		a := &paymentRetrierAdapter{invoiceStore: attemptsOnlyStore{}}
		err := outcome(a, with(domain.InvoiceFinalized, domain.PaymentUnknown, ""))
		if err == nil || errors.Is(err, dunning.ErrPaymentInFlight) {
			t.Fatalf("got %v, want an error that is not ErrPaymentInFlight", err)
		}
		if got := classifyDunningRetryResult(ctx, "inv_1", err); got != dunning.ErrTransientSkip {
			t.Errorf("classified as %v, want an uncounted skip", got)
		}
	})

	t.Run("failed by its webhook first is a counted decline with the attempt's Ask", func(t *testing.T) {
		a := &paymentRetrierAdapter{invoiceStore: attemptsOnlyStore{attempts: []domain.InvoiceChargeAttempt{
			{StripePaymentIntentID: "pi_old", DeclineCode: "lost_card"},
			{StripePaymentIntentID: "pi_race", DeclineCode: "authentication_required"},
		}}}
		inv := with(domain.InvoiceFinalized, domain.PaymentFailed, "pi_race")
		inv.LastPaymentError = "This payment requires authentication. If the customer is off session, ask them to return to your application."
		err := outcome(a, inv)
		var re *dunning.RetryError
		if !errors.As(err, &re) || re.PaymentIntentID != "pi_race" || re.Ask != domain.DeclineAskConfirm || !re.NoReason {
			t.Fatalf("got %#v, want a RetryError on pi_race asking to confirm, with no reason line", err)
		}
		var pe *payment.PaymentError
		if !errors.As(err, &pe) || pe.Unknown {
			t.Fatalf("got %#v, want a definite PaymentError inside", err)
		}
		if got := classifyDunningRetryResult(ctx, "inv_1", err); got != err {
			t.Errorf("classified as %v, want counted as it is", got)
		}
		if err.Error() == inv.LastPaymentError {
			t.Error("the failure's words are the invoice's last_payment_error")
		}
		if releases != 1 {
			t.Errorf("hold released %d times, want once: the round-trip is over", releases)
		}
	})

	t.Run("failed with an unreadable attempt still counts", func(t *testing.T) {
		a := &paymentRetrierAdapter{invoiceStore: attemptsOnlyStore{err: errors.New("db down")}}
		err := outcome(a, with(domain.InvoiceFinalized, domain.PaymentFailed, "pi_race"))
		var re *dunning.RetryError
		if !errors.As(err, &re) || re.Ask != domain.DeclineAskNone {
			t.Fatalf("got %#v, want a counted RetryError asking nothing", err)
		}
	})

	for _, inv := range []domain.Invoice{
		with(domain.InvoiceFinalized, domain.PaymentPending, ""),
		with(domain.InvoiceVoided, domain.PaymentFailed, ""),
		with(domain.InvoiceUncollectible, domain.PaymentPending, ""),
	} {
		t.Run(string(inv.Status)+"/"+string(inv.PaymentStatus)+" is a loud skip", func(t *testing.T) {
			a := &paymentRetrierAdapter{invoiceStore: attemptsOnlyStore{}}
			err := outcome(a, inv)
			if err == nil {
				t.Fatal("got nil: the run would close as recovered over an unpaid invoice")
			}
			if got := classifyDunningRetryResult(ctx, "inv_1", err); got != dunning.ErrTransientSkip {
				t.Errorf("classified as %v, want an uncounted skip", got)
			}
		})
	}
}
