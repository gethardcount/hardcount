package payment

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// A charge Stripe refuses for the key, not the card, is "not attempted"
// (G2 design, approved before go-live). The real trigger is a key rotation
// (the old key now answers 401) or a restricted key missing the PaymentIntent
// write permission (403). Stripe sends both as invalid_request_error, so read
// by type they were a definite failure: the invoice marked failed, a dunning
// attempt used, Stripe's sentence in the customer's email. Every reply here is
// decoded by stripe-go itself through stripeHTTPClient.

const (
	// Shapes of real Stripe replies (the same bodies the stale-hold list read
	// meets, stripe_client_list_test.go).
	stripe401BadKey     = `{"error":{"message":"Invalid API Key provided: sk_live_****abcd","type":"invalid_request_error"}}`
	stripe403Restricted = `{"error":{"message":"The provided key 'rk_live_****abcd' does not have the required permissions for this endpoint on account 'acct_x'. Having the 'rak_payment_intent_write' permission would allow this request to continue.","type":"invalid_request_error"}}`
)

func createParams() PaymentIntentParams {
	return PaymentIntentParams{AmountCents: 100, Currency: "usd", CustomerID: "cus_x", PaymentMethodID: "pm_x", IdempotencyKey: "k"}
}

// assertKeyError checks the error is a skip (ErrPaymentTransient), not a
// decline (no *PaymentError anywhere in it), with the given reason.
func assertKeyError(t *testing.T, err error, reason string) {
	t.Helper()
	if !errors.Is(err, ErrPaymentTransient) {
		t.Fatalf("err = %v, want an ErrPaymentTransient (charge not attempted)", err)
	}
	var pe *PaymentError
	if errors.As(err, &pe) {
		t.Fatalf("err carries *PaymentError %+v: every charge caller would read it as a decline", pe)
	}
	var ke *StripeKeyError
	if !errors.As(err, &ke) || ke.Reason != reason {
		t.Fatalf("err = %#v, want a StripeKeyError with reason %q", err, reason)
	}
	if IsUnknownPaymentFailure(err) {
		t.Errorf("a refused key counted for the shared breaker: one tenant's key would open it for every tenant")
	}
}

func TestCreatePaymentIntent_RefusedKeyIsNotADecline(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		inMsg  string
	}{
		{"401 invalid or rolled key", http.StatusUnauthorized, stripe401BadKey, "Invalid API Key"},
		{"403 restricted key without permission", http.StatusForbidden, stripe403Restricted, "rak_payment_intent_write"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := withCannedStripe(t, tc.status, tc.body)
			c := NewLiveStripeClient(NewStripeClients(fixedCreds{}))
			_, err := c.CreatePaymentIntent(auth.WithTenantID(postgres.WithLivemode(context.Background(), true), "t1"), createParams())
			assertKeyError(t, err, StripeKeyRejected)
			var ke *StripeKeyError
			if errors.As(err, &ke) && !strings.Contains(ke.Message, tc.inMsg) {
				t.Errorf("message %q, want Stripe's sentence (it goes to the log and the Settings notice)", ke.Message)
			}
			if stub.calls != 1 {
				t.Errorf("Stripe called %d times, want 1", stub.calls)
			}
		})
	}
}

func TestCreatePaymentIntent_NoKeyIsNotADecline(t *testing.T) {
	t.Run("no credentials for the mode", func(t *testing.T) {
		stub := withCannedStripe(t, http.StatusOK, `{}`)
		c := NewLiveStripeClient(NewStripeClients(&stubCredentials{}))
		_, err := c.CreatePaymentIntent(auth.WithTenantID(postgres.WithLivemode(context.Background(), true), "t1"), createParams())
		assertKeyError(t, err, StripeKeyNotConfigured)
		if stub.calls != 0 {
			t.Errorf("Stripe called %d times with no key", stub.calls)
		}
	})
	t.Run("no Stripe bundle at all", func(t *testing.T) {
		var c *LiveStripeClient // NewLiveStripeClient's answer with no fetcher
		_, err := c.CreatePaymentIntent(context.Background(), createParams())
		assertKeyError(t, err, StripeKeyNotConfigured)
	})
}

// Controls: a decline stays a definite failure with its code, a 429 stays a
// plain skip, a 500 stays unknown.
func TestCreatePaymentIntent_RefusedKeyControls(t *testing.T) {
	ctx := auth.WithTenantID(postgres.WithLivemode(context.Background(), true), "t1")
	t.Run("402 decline", func(t *testing.T) {
		withCannedStripe(t, http.StatusPaymentRequired, stripeDecline)
		_, err := NewLiveStripeClient(NewStripeClients(fixedCreds{})).CreatePaymentIntent(ctx, createParams())
		var pe *PaymentError
		if !errors.As(err, &pe) || pe.Unknown || pe.DeclineCode != "generic_decline" {
			t.Fatalf("err = %#v, want a definite decline with code generic_decline", err)
		}
		var ke *StripeKeyError
		if errors.As(err, &ke) {
			t.Fatalf("a card decline was read as a refused key")
		}
	})
	t.Run("429", func(t *testing.T) {
		withCannedStripe(t, http.StatusTooManyRequests, stripe429)
		_, err := NewLiveStripeClient(NewStripeClients(fixedCreds{})).CreatePaymentIntent(ctx, createParams())
		var ke *StripeKeyError
		if !errors.Is(err, ErrPaymentTransient) || errors.As(err, &ke) {
			t.Fatalf("err = %#v, want a plain ErrPaymentTransient", err)
		}
	})
	t.Run("500", func(t *testing.T) {
		withCannedStripe(t, http.StatusInternalServerError, stripe500)
		_, err := NewLiveStripeClient(NewStripeClients(fixedCreds{})).CreatePaymentIntent(ctx, createParams())
		var pe *PaymentError
		if !errors.As(err, &pe) || !pe.Unknown {
			t.Fatalf("err = %#v, want an unknown outcome", err)
		}
	})
}

// recordingKeyHealth records MarkKeyRejected and ClearKeyRejected calls.
type recordingKeyHealth struct {
	marks  []string
	clears []string
}

func (r *recordingKeyHealth) ClearKeyRejected(_ context.Context, tenantID string, livemode bool) error {
	mode := "test"
	if livemode {
		mode = "live"
	}
	r.clears = append(r.clears, tenantID+"|"+mode)
	return nil
}

func (r *recordingKeyHealth) MarkKeyRejected(_ context.Context, tenantID string, livemode bool, msg string) error {
	mode := "test"
	if livemode {
		mode = "live"
	}
	r.marks = append(r.marks, tenantID+"|"+mode+"|"+msg)
	return nil
}

// keyUnusableCount reads hardcount_stripe_key_unusable_total{reason}.
func keyUnusableCount(t *testing.T, reason string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "hardcount_stripe_key_unusable_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "reason" && l.GetValue() == reason {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// The charge writes nothing: no attempt row, no outcome stamp (so no failed
// status, no seq bump), no dunning start. It releases its hold, marks the
// key on Settings, counts the metric, and returns the key error for Collect.
func TestChargeInvoice_RefusedKey_WritesNothingAndTellsTheOperator(t *testing.T) {
	withCannedStripe(t, http.StatusUnauthorized, stripe401BadKey)
	invoices := newMockInvoiceUpdater()
	inv := pendingSnapshot()
	invoices.invoices[inv.ID] = inv
	dunning := &recordingDunningStarter{}
	email := &recordingFailedEmail{}
	health := &recordingKeyHealth{}
	s := NewStripe(NewLiveStripeClient(NewStripeClients(fixedCreds{})), invoices, newMockWebhookStore(), nil, dunning)
	s.SetEmailPaymentFailed(email, staticCustomerEmail{})
	s.SetKeyHealth(health)
	before := keyUnusableCount(t, StripeKeyRejected)

	_, err := s.ChargeInvoice(auth.WithTenantID(postgres.WithLivemode(context.Background(), true), "t1"), "t1", inv, testHold, "cus_stripe", "pm_test")
	assertKeyError(t, err, StripeKeyRejected)

	if n := len(invoices.chargeAttempts); n != 0 {
		t.Errorf("charge attempt rows = %d, want 0: nothing was attempted", n)
	}
	if got := invoices.invoices[inv.ID]; got.PaymentStatus != inv.PaymentStatus || got.ChargeAttemptSeq != inv.ChargeAttemptSeq || got.LastPaymentError != "" {
		t.Errorf("invoice %s seq %d error %q, want %s seq %d and no error: nothing was attempted",
			got.PaymentStatus, got.ChargeAttemptSeq, got.LastPaymentError, inv.PaymentStatus, inv.ChargeAttemptSeq)
	}
	if n := len(dunning.calls); n != 0 {
		t.Errorf("dunning started %d times for a refused key", n)
	}
	if n := email.sends; n != 0 {
		t.Errorf("%d payment-failed emails for a refused key", n)
	}
	if got := len(invoices.leaseReleases); got != 1 {
		t.Errorf("hold releases = %d, want 1", got)
	}
	if len(health.marks) != 1 || !strings.HasPrefix(health.marks[0], "t1|live|Invalid API Key") {
		t.Errorf("Settings marks = %v, want one for t1 live with Stripe's sentence", health.marks)
	}
	if got := keyUnusableCount(t, StripeKeyRejected) - before; got != 1 {
		t.Errorf("hardcount_stripe_key_unusable_total{reason=rejected} rose by %v, want 1", got)
	}
	if len(health.clears) != 0 {
		t.Errorf("Settings clears = %v, want none: the key was refused", health.clears)
	}
}

// A charge Stripe accepts proves the stored key works, so the refused-key
// notice is cleared: a restricted key fixed by granting the permission in
// Stripe is the same key, and no Connect runs to clear it.
func TestChargeInvoice_AcceptedCharge_ClearsRefusedKeyNotice(t *testing.T) {
	withCannedStripe(t, http.StatusOK, `{"id":"pi_ok","object":"payment_intent","status":"succeeded","amount":100,"currency":"usd"}`)
	invoices := newMockInvoiceUpdater()
	inv := pendingSnapshot()
	invoices.invoices[inv.ID] = inv
	health := &recordingKeyHealth{}
	s := NewStripe(NewLiveStripeClient(NewStripeClients(fixedCreds{})), invoices, newMockWebhookStore(), nil, &recordingDunningStarter{})
	s.SetKeyHealth(health)

	if _, err := s.ChargeInvoice(auth.WithTenantID(postgres.WithLivemode(context.Background(), true), "t1"), "t1", inv, testHold, "cus_stripe", "pm_test"); err != nil {
		t.Fatalf("charge: %v", err)
	}
	if len(health.clears) != 1 || health.clears[0] != "t1|live" {
		t.Errorf("Settings clears = %v, want one for t1 live", health.clears)
	}
	if len(health.marks) != 0 {
		t.Errorf("Settings marks = %v, want none", health.marks)
	}
}

// No key connected for the mode: the same "not attempted", nothing to mark.
func TestChargeInvoice_NoKey_WritesNothing(t *testing.T) {
	withCannedStripe(t, http.StatusOK, `{}`)
	invoices := newMockInvoiceUpdater()
	inv := pendingSnapshot()
	invoices.invoices[inv.ID] = inv
	dunning := &recordingDunningStarter{}
	health := &recordingKeyHealth{}
	s := NewStripe(NewLiveStripeClient(NewStripeClients(&stubCredentials{})), invoices, newMockWebhookStore(), nil, dunning)
	s.SetKeyHealth(health)
	before := keyUnusableCount(t, StripeKeyNotConfigured)

	_, err := s.ChargeInvoice(auth.WithTenantID(postgres.WithLivemode(context.Background(), true), "t1"), "t1", inv, testHold, "cus_stripe", "pm_test")
	assertKeyError(t, err, StripeKeyNotConfigured)
	if n := len(invoices.chargeAttempts); n != 0 {
		t.Errorf("charge attempt rows = %d, want 0", n)
	}
	if got := invoices.invoices[inv.ID]; got.PaymentStatus != inv.PaymentStatus {
		t.Errorf("payment status %s, want %s", got.PaymentStatus, inv.PaymentStatus)
	}
	if n := len(dunning.calls); n != 0 {
		t.Errorf("dunning started %d times with no key", n)
	}
	if got := len(invoices.leaseReleases); got != 1 {
		t.Errorf("hold releases = %d, want 1", got)
	}
	if len(health.marks) != 0 {
		t.Errorf("Settings marks = %v, want none: there is no key row to mark", health.marks)
	}
	if got := keyUnusableCount(t, StripeKeyNotConfigured) - before; got != 1 {
		t.Errorf("hardcount_stripe_key_unusable_total{reason=not_configured} rose by %v, want 1", got)
	}
}
