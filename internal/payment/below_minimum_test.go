package payment

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/payment/breaker"
)

// Stripe refuses a charge below its minimum (USD 0.50) with this reply, seen
// on a test-mode account on 2026-10-22: HTTP 400, invalid_request_error, code
// amount_too_small, and no PaymentIntent. Every reply here is decoded by
// stripe-go itself through stripeHTTPClient.
const stripeAmountTooSmall = `{"error":{"code":"amount_too_small","doc_url":"https://stripe.com/docs/error-codes/amount-too-small","message":"Amount must be at least $0.50 usd","param":"amount","type":"invalid_request_error"}}`

// A 400 with another code stays the definite failure it was (control).
const stripeBadParam = `{"error":{"code":"parameter_invalid_integer","message":"Invalid integer: abc","param":"amount","type":"invalid_request_error"}}`

func TestCreatePaymentIntent_BelowMinimumIsItsOwnOutcome(t *testing.T) {
	ctx := auth.WithTenantID(context.Background(), "t1")
	withCannedStripe(t, http.StatusBadRequest, stripeAmountTooSmall)
	_, err := NewLiveStripeClient(NewStripeClients(fixedCreds{})).CreatePaymentIntent(ctx, createParams())

	var bm *BelowMinimumError
	if !errors.As(err, &bm) || !strings.Contains(bm.Message, "at least $0.50") {
		t.Fatalf("err = %#v, want a BelowMinimumError carrying Stripe's sentence", err)
	}
	if !errors.Is(err, domain.ErrBelowMinimumCharge) {
		t.Errorf("err does not match domain.ErrBelowMinimumCharge, which billing and dunning test for")
	}
	var pe *PaymentError
	if errors.As(err, &pe) {
		t.Errorf("err carries *PaymentError %+v: every charge caller would read it as a decline", pe)
	}
	if errors.Is(err, ErrPaymentTransient) {
		t.Errorf("err is a transient skip: the invoice would stay queued and Stripe be asked every tick")
	}
	if IsUnknownPaymentFailure(err) {
		t.Errorf("a below-minimum refusal counted for the shared breaker")
	}

	t.Run("another 400 stays a definite failure", func(t *testing.T) {
		withCannedStripe(t, http.StatusBadRequest, stripeBadParam)
		_, err := NewLiveStripeClient(NewStripeClients(fixedCreds{})).CreatePaymentIntent(ctx, createParams())
		var pe *PaymentError
		if !errors.As(err, &pe) || pe.Unknown || errors.Is(err, domain.ErrBelowMinimumCharge) {
			t.Fatalf("err = %#v, want a definite PaymentError that is not below-minimum", err)
		}
	})
}

// Whichever charge gets the refusal (the auto-charge sweep or finalize,
// Collect payment, a dunning retry), the invoice stays open and unpaid, out of
// the auto-charge queue and marked for the operator; nothing starts dunning or
// emails the customer; the hold is released; and a payment recovery running
// on it ends, by dunning itself for its own retry and through the resolver
// for the rest.
func TestChargeInvoice_BelowMinimum_NotADecline(t *testing.T) {
	type charge func(s *Stripe, ctx context.Context, inv domain.Invoice) (domain.Invoice, error)
	for _, tc := range []struct {
		name        string
		charge      charge
		trigger     domain.ChargeAttemptTrigger
		resolveHere bool
		from        domain.InvoicePaymentStatus
	}{
		{"auto-charge", func(s *Stripe, ctx context.Context, inv domain.Invoice) (domain.Invoice, error) {
			return s.ChargeInvoice(ctx, "t1", inv, testHold, "cus_stripe", "pm_test")
		}, domain.ChargeTriggerAutoCharge, true, domain.PaymentPending},
		{"collect payment on a failed invoice", func(s *Stripe, ctx context.Context, inv domain.Invoice) (domain.Invoice, error) {
			return s.ChargeInvoiceNow(ctx, "t1", inv, testHold, "cus_stripe", "pm_test")
		}, domain.ChargeTriggerChargeNow, true, domain.PaymentFailed},
		{"dunning retry", func(s *Stripe, ctx context.Context, inv domain.Invoice) (domain.Invoice, error) {
			return s.ChargeInvoiceForDunningRetry(ctx, "t1", inv, testHold, "cus_stripe", "pm_test")
		}, domain.ChargeTriggerDunningRetry, false, domain.PaymentFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withCannedStripe(t, http.StatusBadRequest, stripeAmountTooSmall)
			invoices := newMockInvoiceUpdater()
			inv := pendingSnapshot()
			inv.AmountDueCents = 30
			inv.PaymentStatus = tc.from
			inv.AutoChargePending = tc.from == domain.PaymentPending
			inv.ChargeAttemptSeq = 4
			invoices.invoices[inv.ID] = inv
			starter := &recordingDunningStarter{}
			resolver := &recordingDunningResolver{}
			emails := &recordingFailedEmail{}
			s := NewStripe(NewLiveStripeClient(NewStripeClients(fixedCreds{})), invoices, newMockWebhookStore(), nil, starter)
			s.SetDunningResolver(resolver)
			s.SetEmailPaymentFailed(emails, staticCustomerEmail{})

			_, err := tc.charge(s, auth.WithTenantID(context.Background(), "t1"), inv)
			if !errors.Is(err, domain.ErrBelowMinimumCharge) {
				t.Fatalf("err = %v, want ErrBelowMinimumCharge", err)
			}
			got := invoices.invoices[inv.ID]
			if got.PaymentStatus != domain.PaymentPending || got.AutoChargePending || got.BelowMinimumAt == nil {
				t.Errorf("invoice payment_status=%s queued=%v below_minimum_at=%v, want pending, not queued, stamped",
					got.PaymentStatus, got.AutoChargePending, got.BelowMinimumAt)
			}
			if !strings.Contains(got.LastPaymentError, "at least $0.50") || got.ChargeAttemptSeq != 5 {
				t.Errorf("last_payment_error %q seq %d, want Stripe's sentence and the seq moved to 5", got.LastPaymentError, got.ChargeAttemptSeq)
			}
			if len(starter.calls) != 0 {
				t.Errorf("StartDunning called %d times, want none", len(starter.calls))
			}
			if emails.sends != 0 {
				t.Errorf("payment-failed emails %d, want none", emails.sends)
			}
			if len(invoices.leaseReleases) != 1 {
				t.Errorf("hold releases %d, want 1: the answer is final", len(invoices.leaseReleases))
			}
			if n := len(invoices.chargeAttempts); n != 1 || invoices.chargeAttempts[0].Outcome != domain.ChargeAttemptFailed ||
				invoices.chargeAttempts[0].Trigger != tc.trigger || !strings.Contains(invoices.chargeAttempts[0].ProviderReason, "at least $0.50") {
				t.Errorf("attempt rows %+v, want one failed %s attempt with Stripe's sentence", invoices.chargeAttempts, tc.trigger)
			}
			switch {
			case tc.resolveHere && (len(resolver.calls) != 1 || resolver.calls[0].resolution != domain.ResolutionAmountBelowMinimum):
				t.Errorf("dunning resolves %+v, want one amount_below_minimum", resolver.calls)
			case !tc.resolveHere && len(resolver.calls) != 0:
				t.Errorf("dunning resolves %+v, want none: dunning ends its own run at the retry's instant", resolver.calls)
			}
		})
	}
}

// One tenant's sub-minimum invoices must not open the breaker every tenant
// shares: the refusal is about the amount, not an outage.
func TestChargeInvoice_BelowMinimum_LeavesTheBreakerClosed(t *testing.T) {
	withCannedStripe(t, http.StatusBadRequest, stripeAmountTooSmall)
	invoices := newMockInvoiceUpdater()
	s := NewStripe(NewLiveStripeClient(NewStripeClients(fixedCreds{})), invoices, newMockWebhookStore(), nil)
	b := breaker.New(breaker.Config{Countable: IsUnknownPaymentFailure, FailureThreshold: 3, Cooldown: time.Hour})
	s.SetBreaker(b)
	for i := 0; i < 6; i++ {
		inv := pendingSnapshot()
		inv.ID = "inv_" + string(rune('a'+i))
		inv.AmountDueCents = 30
		invoices.invoices[inv.ID] = inv
		if _, err := s.ChargeInvoice(auth.WithTenantID(context.Background(), "t1"), "t1", inv, testHold, "cus_stripe", "pm_test"); !errors.Is(err, domain.ErrBelowMinimumCharge) {
			t.Fatalf("charge %d: err = %v, want ErrBelowMinimumCharge", i+1, err)
		}
	}
	if st := b.State(); st != breaker.StateClosed {
		t.Errorf("breaker is %s after six below-minimum refusals, want closed", st)
	}
}

// The hosted page's Checkout Session refusal (api/adapters.go) is read the
// same way, and IsDefiniteRefusal separates a refusal that made nothing
// from an answer that may hide a session.
func TestBelowMinimumOf_AndIsDefiniteRefusal(t *testing.T) {
	ctx := auth.WithTenantID(context.Background(), "t1")
	// The raw stripe-go error, as api/adapters.go gets it from the create.
	refusal := func(status int, body string) error {
		t.Helper()
		withCannedStripe(t, status, body)
		sc, err := NewStripeClients(fixedCreds{}).ResolveCtx(ctx)
		if err != nil {
			t.Fatalf("stripe client: %v", err)
		}
		_, rawErr := sc.V1CheckoutSessions.Retrieve(ctx, "cs_x", nil)
		return rawErr
	}
	tooSmall := refusal(http.StatusBadRequest, stripeAmountTooSmall)
	if bm := BelowMinimumOf(tooSmall); bm == nil || !strings.Contains(bm.Message, "at least $0.50") {
		t.Errorf("BelowMinimumOf(amount_too_small) = %v, want Stripe's sentence", bm)
	}
	if !IsDefiniteRefusal(tooSmall) {
		t.Error("a 400 amount_too_small made nothing; it is a definite refusal")
	}
	if BelowMinimumOf(refusal(http.StatusBadRequest, stripeBadParam)) != nil {
		t.Error("another 400 read as below-minimum")
	}
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"409 key in use", http.StatusConflict, `{"error":{"type":"invalid_request_error","message":"There is currently another in-progress request using this Stripe API key"}}`},
		{"429", http.StatusTooManyRequests, stripe429},
		{"500", http.StatusInternalServerError, `{"error":{"type":"api_error","message":"An unknown error occurred"}}`},
		{"idempotency_error", http.StatusBadRequest, `{"error":{"type":"idempotency_error","message":"Keys for idempotent requests can only be used with the same parameters they were first used with."}}`},
	} {
		if IsDefiniteRefusal(refusal(tc.status, tc.body)) {
			t.Errorf("%s read as a definite refusal: a session may exist", tc.name)
		}
	}
	if IsDefiniteRefusal(errors.New("dial tcp: connection reset")) {
		t.Error("a network error read as a definite refusal")
	}
}
