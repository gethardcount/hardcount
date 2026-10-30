package api

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/dunning"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// stripeConnectedForTest answers the retrier's Stripe-connected check. The
// integration rigs that build paymentRetrierAdapter by hand charge through a
// Stripe stub, so they wire it connected.
type stripeConnectedForTest struct {
	connected bool
	err       error
}

func (s stripeConnectedForTest) HasFor(context.Context, string, bool) (bool, error) {
	return s.connected, s.err
}

// noStripeRetryStore is the invoice store a no-Stripe retry reaches: it
// claims and releases the hold, and fails the test on anything a retry that
// charges nothing must not call.
type noStripeRetryStore struct {
	t        *testing.T
	inv      domain.Invoice
	released int
}

func (s *noStripeRetryStore) Get(context.Context, string, string) (domain.Invoice, error) {
	return s.inv, nil
}

func (s *noStripeRetryStore) ClaimChargeForDunningRetry(context.Context, string, string) (domain.ChargeHold, error) {
	return domain.ChargeHold{Until: time.Now().Add(5 * time.Minute)}, nil
}

func (s *noStripeRetryStore) ReleaseChargeHold(context.Context, string, string, domain.ChargeHold) error {
	s.released++
	return nil
}

func (s *noStripeRetryStore) MarkPaid(context.Context, string, string, string, time.Time) (domain.Invoice, error) {
	s.t.Fatal("MarkPaid called on a retry that has nothing to settle")
	return domain.Invoice{}, nil
}

func (s *noStripeRetryStore) SettleZeroDue(context.Context, string, string, time.Time) (domain.Invoice, bool, error) {
	s.t.Fatal("SettleZeroDue called on an invoice with money due")
	return domain.Invoice{}, false, nil
}

func (s *noStripeRetryStore) RetryBlockingDecline(context.Context, string, string, string) (string, error) {
	return "", nil
}

func (s *noStripeRetryStore) ListChargeAttemptsByInvoice(context.Context, string, string) ([]domain.InvoiceChargeAttempt, error) {
	return nil, nil
}

// countingPaymentSetups records whether the retry read the customer's card.
type countingPaymentSetups struct{ reads int }

func (p *countingPaymentSetups) UpsertPaymentSetup(context.Context, string, domain.CustomerPaymentSetup) (domain.CustomerPaymentSetup, error) {
	return domain.CustomerPaymentSetup{}, errors.New("not used")
}

func (p *countingPaymentSetups) UpsertPaymentSetupAudited(context.Context, string, domain.CustomerPaymentSetup, func(tx *sql.Tx) error) (domain.CustomerPaymentSetup, error) {
	return domain.CustomerPaymentSetup{}, errors.New("not used")
}

func (p *countingPaymentSetups) GetPaymentSetup(context.Context, string, string) (domain.CustomerPaymentSetup, error) {
	p.reads++
	// A card on file from before Stripe was disconnected: the no-Stripe
	// retry must not charge it.
	return domain.CustomerPaymentSetup{StripeCustomerID: "cus_old", StripePaymentMethodID: "pm_old"}, nil
}

// Without Stripe for the invoice's mode a dunning retry is a reminder only
// (ADR-060 amendment 2026-10-30): it charges nothing, even with a card on
// file from before, and reports ErrNoOnlinePaymentOnRetry, which counts as an
// attempt and asks the customer to pay the invoice. A failed read of the
// connection skips the tick uncounted, never "not connected". Either way the
// hold is released and the card is not read. The charger is nil, so reaching
// a charge panics. Mutations: drop the !connected return (the card is read
// and charged); treat a read error as not connected (a counted attempt).
func TestDunningRetry_NoStripe_ChargesNothing(t *testing.T) {
	ctx := postgres.WithLivemode(context.Background(), false)
	inv := domain.Invoice{ID: "inv_ns", TenantID: "t1", CustomerID: "cus_1", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentFailed, AmountDueCents: 5000}
	cases := []struct {
		name  string
		ready stripeConnectedForTest
		want  error
	}{
		{"not connected", stripeConnectedForTest{}, domain.ErrNoOnlinePaymentOnRetry},
		{"read error", stripeConnectedForTest{err: errors.New("credentials read failed")}, dunning.ErrTransientSkip},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &noStripeRetryStore{t: t, inv: inv}
			setups := &countingPaymentSetups{}
			a := &paymentRetrierAdapter{invoiceStore: store, paymentSetups: setups, stripeReady: tc.ready}
			err := a.RetryPayment(ctx, "t1", inv.ID, inv.CustomerID)
			if !errors.Is(err, tc.want) {
				t.Fatalf("RetryPayment = %v, want %v", err, tc.want)
			}
			if store.released != 1 {
				t.Errorf("hold released %d times, want 1", store.released)
			}
			if setups.reads != 0 {
				t.Errorf("payment setup read %d times, want 0: nothing can be charged", setups.reads)
			}
		})
	}

	// Counted, and seen as a no-card retry (so the timeline and the attempt
	// count treat it as one), but a reminder of its own.
	if got := classifyDunningRetryResult(ctx, inv.ID, domain.ErrNoOnlinePaymentOnRetry); got != domain.ErrNoOnlinePaymentOnRetry {
		t.Fatalf("classify = %v, want the error itself (counted)", got)
	}
	if !errors.Is(domain.ErrNoOnlinePaymentOnRetry, domain.ErrNoPaymentMethodOnRetry) {
		t.Fatal("ErrNoOnlinePaymentOnRetry must be a kind of ErrNoPaymentMethodOnRetry")
	}
}

// With Stripe connected the retry reads the card as before: no card is the
// add-a-card case.
func TestDunningRetry_StripeConnected_ReadsTheCard(t *testing.T) {
	ctx := postgres.WithLivemode(context.Background(), false)
	inv := domain.Invoice{ID: "inv_c", TenantID: "t1", CustomerID: "cus_1", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentFailed, AmountDueCents: 5000}
	store := &noStripeRetryStore{t: t, inv: inv}
	a := &paymentRetrierAdapter{invoiceStore: store, paymentSetups: &noCardSetups{}, stripeReady: stripeConnectedForTest{connected: true}}
	err := a.RetryPayment(ctx, "t1", inv.ID, inv.CustomerID)
	if !errors.Is(err, domain.ErrNoPaymentMethodOnRetry) || errors.Is(err, domain.ErrNoOnlinePaymentOnRetry) {
		t.Fatalf("RetryPayment = %v, want ErrNoPaymentMethodOnRetry (no card), not the no-Stripe reminder", err)
	}
}

// noCardSetups: the customer has no card.
type noCardSetups struct{ countingPaymentSetups }

func (*noCardSetups) GetPaymentSetup(context.Context, string, string) (domain.CustomerPaymentSetup, error) {
	return domain.CustomerPaymentSetup{}, nil
}
