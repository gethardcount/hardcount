package payment

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/payment/breaker"
)

// A charge that sends nothing to Stripe releases its hold (ADR-127). The
// breaker refusing the call and credentials that could not be read both stop
// the charge before any request leaves, so the hold speaks for no lost
// charge. Kept, it blocked the invoice (every charge, the hosted page, void,
// offline payment, dunning) until the stale-hold sweep asked Stripe: an hour
// or more, for every invoice charged while the shared breaker was open.
//
// A 429 is the control: stripe-go may have resent the request after a
// network error, and the first request can still be running at Stripe, so
// that hold stays for the sweep.
func TestChargeInvoice_NothingSent_ReleasesTheHold(t *testing.T) {
	t.Run("breaker open", func(t *testing.T) {
		client := &mockStripeClient{shouldFail: true, failErr: &PaymentError{Message: "An unknown error occurred", Unknown: true}}
		invoices := newMockInvoiceUpdater()
		inv := pendingSnapshot()
		invoices.invoices[inv.ID] = inv
		s := NewStripe(client, invoices, newMockWebhookStore(), nil)
		b := breaker.New(breaker.Config{Countable: IsUnknownPaymentFailure, FailureThreshold: 5, Cooldown: time.Hour})
		s.SetBreaker(b)
		for i := 0; i < 5; i++ {
			if _, err := s.ChargeInvoice(context.Background(), "t1", inv, testHold, "cus_stripe", "pm_test"); err == nil {
				t.Fatalf("setup: charge %d succeeded, want an unclear outcome", i+1)
			}
		}
		if st := b.State(); st != breaker.StateOpen {
			t.Fatalf("setup: breaker is %s after five unclear outcomes, want open", st)
		}
		creates, releases := client.createCalls, len(invoices.leaseReleases)

		_, err := s.ChargeInvoice(context.Background(), "t1", inv, testHold, "cus_stripe", "pm_test")
		if !errors.Is(err, ErrPaymentTransient) {
			t.Fatalf("err = %v, want ErrPaymentTransient", err)
		}
		if client.createCalls != creates {
			t.Fatalf("Stripe received %d creates while the breaker was open", client.createCalls-creates)
		}
		if got := len(invoices.leaseReleases) - releases; got != 1 {
			t.Errorf("releases = %d, want 1: nothing was sent, so the hold must not wait for the stale-hold sweep", got)
		}
	})

	t.Run("credentials could not be read", func(t *testing.T) {
		stub := withRoutedStripe(t, map[string]func(*http.Request) (int, string){})
		creds := &stubCredentials{err: map[string]error{"t1|live": errors.New("read tenant stripe secret: connection reset by peer")}}
		invoices := newMockInvoiceUpdater()
		inv := pendingSnapshot()
		invoices.invoices[inv.ID] = inv
		s := NewStripe(NewLiveStripeClient(NewStripeClients(creds)), invoices, newMockWebhookStore(), nil)

		_, err := s.ChargeInvoice(auth.WithTenantID(context.Background(), "t1"), "t1", inv, testHold, "cus_stripe", "pm_test")
		if !errors.Is(err, ErrPaymentTransient) {
			t.Fatalf("err = %v, want ErrPaymentTransient", err)
		}
		if n := len(stub.requests); n != 0 {
			t.Fatalf("Stripe received %d requests without credentials", n)
		}
		if got := len(invoices.leaseReleases); got != 1 {
			t.Errorf("releases = %d, want 1: nothing was sent, so the hold must not wait for the stale-hold sweep", got)
		}
	})

	t.Run("429 keeps the hold (control)", func(t *testing.T) {
		withCannedStripe(t, http.StatusTooManyRequests, stripe429)
		invoices := newMockInvoiceUpdater()
		inv := pendingSnapshot()
		invoices.invoices[inv.ID] = inv
		s := NewStripe(NewLiveStripeClient(NewStripeClients(fixedCreds{})), invoices, newMockWebhookStore(), nil)

		_, err := s.ChargeInvoice(auth.WithTenantID(context.Background(), "t1"), "t1", inv, testHold, "cus_stripe", "pm_test")
		if !errors.Is(err, ErrPaymentTransient) {
			t.Fatalf("err = %v, want ErrPaymentTransient", err)
		}
		if got := len(invoices.leaseReleases); got != 0 {
			t.Errorf("releases = %d, want 0: a 429 can follow a first request Stripe is still running, so the hold stays for the sweep", got)
		}
	})
}
