package payment

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/tenantstripe"
)

type fixedCreds struct{}

func (fixedCreds) GetPlaintext(context.Context, string, bool) (tenantstripe.PlaintextSecrets, error) {
	return tenantstripe.PlaintextSecrets{SecretKey: "sk_test_FAKE_rate_limit"}, nil
}

// cannedStripe answers every request with one fixed HTTP response.
type cannedStripe struct {
	status int
	body   string
	calls  int
}

func (c *cannedStripe) RoundTrip(*http.Request) (*http.Response, error) {
	c.calls++
	return &http.Response{
		StatusCode: c.status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(c.body)),
	}, nil
}

// The body is a real Stripe test-mode reply captured on 2026-10-14 by sending
// 150 PaymentIntent creates at once: 90 came back exactly like this.
const stripe429 = `{"error":{"code":"rate_limit","doc_url":"https://stripe.com/docs/error-codes/rate-limit","message":"Request rate limit exceeded. You can learn more about rate limits here https://stripe.com/docs/rate-limits.","type":"rate_limit_error"}}`

const stripeDecline = `{"error":{"code":"card_declined","decline_code":"generic_decline","message":"Your card was declined.","type":"card_error"}}`

func withCannedStripe(t *testing.T, status int, body string) *cannedStripe {
	t.Helper()
	c := &cannedStripe{status: status, body: body}
	old := stripeHTTPClient.Transport
	stripeHTTPClient.Transport = c
	t.Cleanup(func() { stripeHTTPClient.Transport = old })
	return c
}

// A rate-limited charge never ran, so it must be "try again next tick": the
// same signal as the breaker short-circuit, which writes no invoice state,
// spends no dunning attempt and sends no email. Before the fix it fell
// through to "outcome unknown" and parked the invoice for a person.
func TestCreatePaymentIntent_RateLimitIsTransient(t *testing.T) {
	stub := withCannedStripe(t, http.StatusTooManyRequests, stripe429)
	ctx := auth.WithTenantID(context.Background(), "t1")
	c := NewLiveStripeClient(NewStripeClients(fixedCreds{}))

	_, err := c.CreatePaymentIntent(ctx, PaymentIntentParams{AmountCents: 100, Currency: "usd", CustomerID: "cus_x", PaymentMethodID: "pm_x", IdempotencyKey: "k"})
	if !errors.Is(err, ErrPaymentTransient) {
		t.Fatalf("err = %v, want ErrPaymentTransient", err)
	}
	if IsUnknownPaymentFailure(err) {
		t.Fatalf("a rate limit was reported as an unknown outcome: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("Stripe was called %d times, want 1 (stripe-go does not retry a rate_limit 429)", stub.calls)
	}
}

// Control: a real decline is still a definite failure, not a skip.
func TestCreatePaymentIntent_DeclineStaysDefinite(t *testing.T) {
	withCannedStripe(t, http.StatusPaymentRequired, stripeDecline)
	ctx := auth.WithTenantID(context.Background(), "t1")
	c := NewLiveStripeClient(NewStripeClients(fixedCreds{}))

	_, err := c.CreatePaymentIntent(ctx, PaymentIntentParams{AmountCents: 100, Currency: "usd", CustomerID: "cus_x", PaymentMethodID: "pm_x", IdempotencyKey: "k"})
	if errors.Is(err, ErrPaymentTransient) {
		t.Fatalf("a decline was treated as a transient skip: %v", err)
	}
	var pe *PaymentError
	if !errors.As(err, &pe) || pe.Unknown || pe.DeclineCode != "generic_decline" {
		t.Fatalf("err = %#v, want a definite decline with code generic_decline", err)
	}
}
