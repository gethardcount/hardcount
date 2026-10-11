package payment

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/tenantstripe"
)

var errBlip = errors.New("connection reset")

type credsFetcher struct{ err error }

func (f credsFetcher) GetPlaintext(context.Context, string, bool) (tenantstripe.PlaintextSecrets, error) {
	return tenantstripe.PlaintextSecrets{}, f.err
}

// A credential READ error is a retryable "nothing reached Stripe", not the
// definite "Stripe not configured" that spent a dunning attempt. No
// credentials is still "not configured".
func TestStripeClient_CredentialReadErrorIsTransient(t *testing.T) {
	ctx := auth.WithTenantID(context.Background(), "t1")
	c := NewLiveStripeClient(NewStripeClients(credsFetcher{err: errBlip}))
	_, err := c.CreatePaymentIntent(ctx, PaymentIntentParams{})
	if !errors.Is(err, ErrPaymentTransient) || errors.Is(err, ErrStripeNotConfigured) {
		t.Fatalf("read error: err = %v, want ErrPaymentTransient", err)
	}
	c = NewLiveStripeClient(NewStripeClients(credsFetcher{err: errs.ErrNotFound}))
	if _, err := c.CreatePaymentIntent(ctx, PaymentIntentParams{}); !errors.Is(err, ErrStripeNotConfigured) {
		t.Fatalf("no credentials: err = %v, want ErrStripeNotConfigured", err)
	}
}

type cardFetcherStub struct{ err error }

func (c cardFetcherStub) FetchCardDetails(context.Context, string) (CardDetails, error) {
	return CardDetails{}, c.err
}
func (c cardFetcherStub) FetchCardForPaymentIntent(context.Context, string) (CardDetails, error) {
	return CardDetails{}, c.err
}

type nopSetups struct{}

func (nopSetups) UpsertPaymentSetup(_ context.Context, _ string, ps domain.CustomerPaymentSetup) (domain.CustomerPaymentSetup, error) {
	return ps, nil
}
func (nopSetups) UpsertPaymentSetupAudited(_ context.Context, _ string, ps domain.CustomerPaymentSetup, _ func(*sql.Tx) error) (domain.CustomerPaymentSetup, error) {
	return ps, nil
}
func (nopSetups) GetPaymentSetup(context.Context, string, string) (domain.CustomerPaymentSetup, error) {
	return domain.CustomerPaymentSetup{}, nil
}

// Only "the customer saved no card" means there is nothing to record. A
// failed lookup used to read the same way, so the webhook was acked, the
// hosted sweep marked the session done, and the card was never recorded.
func TestRecordCheckoutCard_LookupErrorFailsNoCardSkips(t *testing.T) {
	s := NewStripe(nil, nil, nil, nopSetups{})
	s.SetCardFetcher(cardFetcherStub{err: errBlip})
	if err := s.RecordCheckoutCard(context.Background(), "t1", "cus_1", "cus_stripe", PurposeHostedInvoicePay); !errors.Is(err, errBlip) {
		t.Fatalf("lookup error: err = %v, want it returned", err)
	}
	s.SetCardFetcher(cardFetcherStub{err: ErrNoSavedCard})
	if err := s.RecordCheckoutCard(context.Background(), "t1", "cus_1", "cus_stripe", PurposeHostedInvoicePay); err != nil {
		t.Fatalf("no saved card: err = %v, want nil", err)
	}
}

// A credentials read error reaches the charge as ErrPaymentTransient: nothing
// was sent to Stripe, so the invoice is left as it was and the caller retries.
// Classified as an unknown outcome it parked the invoice for a reconciler that
// had no PaymentIntent to find.
func TestChargeInvoice_CredentialReadErrorLeavesInvoiceUntouched(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	snapshot := pendingSnapshot()
	invoices.invoices["inv_1"] = snapshot
	transient := fmt.Errorf("%w: stripe credential lookup: %v", ErrPaymentTransient, errBlip)
	s := NewStripe(&mockStripeClient{shouldFail: true, failErr: transient}, invoices, newMockWebhookStore(), nil, nil)

	_, err := s.ChargeInvoice(context.Background(), "t1", snapshot, "cus_stripe", "pm_test")
	if !errors.Is(err, ErrPaymentTransient) {
		t.Fatalf("err = %v, want ErrPaymentTransient", err)
	}
	if got := invoices.invoices["inv_1"]; got.PaymentStatus != domain.PaymentPending {
		t.Fatalf("payment_status = %s, want pending (untouched)", got.PaymentStatus)
	}
	if IsUnknownPaymentFailure(err) {
		t.Error("a credentials read error counted as an unknown payment outcome")
	}
}
