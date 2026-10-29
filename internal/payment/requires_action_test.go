package payment

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// stripeAuthNotHandled is Stripe's documented answer to a payment the bank
// wants confirmed when error_on_requires_action is set
// (https://docs.stripe.com/payments/without-card-authentication, "Make a
// payment"): HTTP 402, a card_error with code authentication_required and
// decline code authentication_not_handled. The PaymentIntent is left in
// requires_payment_method, so nothing waits on the customer.
const stripeAuthNotHandled = `{"error":{"code":"authentication_required","decline_code":"authentication_not_handled","doc_url":"https://docs.stripe.com/error-codes#authentication-required","message":"This payment required an authentication action to complete, but ` + "`error_on_requires_action`" + ` was set.","payment_intent":{"id":"pi_1G8JtxDpqHItWkFAnB32FhtI","object":"payment_intent","amount":1099,"status":"requires_payment_method","last_payment_error":{"code":"authentication_required","decline_code":"authentication_not_handled","type":"card_error"}},"type":"card_error"}}`

// capturingStripe answers like cannedStripe and keeps each request's form.
type capturingStripe struct {
	cannedStripe
	forms []url.Values
}

func (c *capturingStripe) RoundTrip(r *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, err
	}
	c.forms = append(c.forms, form)
	return c.cannedStripe.RoundTrip(r)
}

// An off-session charge must never leave a PaymentIntent waiting for a
// customer who cannot reach it (ADR-107, ADR-127 follow-up C). The charge
// asks Stripe to fail such a payment, and the failure is read as the bank's
// "confirm the payment" decline: a definite failure, whose email asks the
// customer to confirm on the hosted invoice page and whose next dunning retry
// does not charge the same card again (domain.RetryBlock).
func TestCreatePaymentIntent_PaymentNeedingTheCustomerFailsAsConfirmDecline(t *testing.T) {
	stub := &capturingStripe{cannedStripe: cannedStripe{status: http.StatusPaymentRequired, body: stripeAuthNotHandled}}
	old := stripeHTTPClient.Transport
	stripeHTTPClient.Transport = stub
	t.Cleanup(func() { stripeHTTPClient.Transport = old })

	ctx := auth.WithTenantID(postgres.WithLivemode(context.Background(), true), "t1")
	c := NewLiveStripeClient(NewStripeClients(fixedCreds{}))
	_, err := c.CreatePaymentIntent(ctx, PaymentIntentParams{AmountCents: 1099, Currency: "usd", CustomerID: "cus_x", PaymentMethodID: "pm_x", IdempotencyKey: "k"})

	if len(stub.forms) != 1 {
		t.Fatalf("requests to Stripe = %d, want 1", len(stub.forms))
	}
	for field, want := range map[string]string{"confirm": "true", "off_session": "true", "error_on_requires_action": "true"} {
		if got := stub.forms[0].Get(field); got != want {
			t.Errorf("PaymentIntent create sent %s=%q, want %q", field, got, want)
		}
	}

	var pe *PaymentError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %#v, want a *PaymentError", err)
	}
	if pe.Unknown || IsUnknownPaymentFailure(err) {
		t.Fatalf("a refused payment was read as an unclear outcome: %v", err)
	}
	if errors.Is(err, ErrPaymentTransient) {
		t.Fatalf("a refused payment was read as transient: %v", err)
	}
	if pe.DeclineCode != domain.AuthenticationRequiredCode {
		t.Errorf("DeclineCode = %q, want %q", pe.DeclineCode, domain.AuthenticationRequiredCode)
	}
	if got := domain.DeclineAskOf(pe.DeclineCode); got != domain.DeclineAskConfirm {
		t.Errorf("DeclineAskOf = %q, want %q: the email must ask the customer to confirm the payment", got, domain.DeclineAskConfirm)
	}
	if !errors.Is(domain.RetryBlock(pe.DeclineCode), domain.ErrCardNeedsConfirmation) {
		t.Errorf("RetryBlock(%q) does not stop dunning from charging the card again", pe.DeclineCode)
	}
	if !strings.Contains(pe.PaymentIntentID, "pi_") {
		t.Errorf("PaymentIntentID = %q, want the refused PaymentIntent's id", pe.PaymentIntentID)
	}
}

// A decline code the bank sent stays as it is: only the
// authentication_required error code is read as one.
func TestDeclineCodeOf_KeepsOtherDeclineCodes(t *testing.T) {
	for _, tc := range []struct{ errType, code, decline, want string }{
		{"card_error", "authentication_required", "authentication_not_handled", "authentication_required"},
		{"card_error", "authentication_required", "", "authentication_required"},
		{"card_error", "card_declined", "insufficient_funds", "insufficient_funds"},
		{"card_error", "card_declined", "generic_decline", "generic_decline"},
		{"invalid_request_error", "authentication_required", "", ""},
	} {
		if got := declineCodeOf(tc.errType, tc.code, tc.decline); got != tc.want {
			t.Errorf("declineCodeOf(%q, %q, %q) = %q, want %q", tc.errType, tc.code, tc.decline, got, tc.want)
		}
	}
}
