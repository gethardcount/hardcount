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
	for field, want := range map[string]string{"confirm": "true", "off_session": "true", "error_on_requires_action": "true", "payment_method_types[0]": "card"} {
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

// stripeDashboardMethodsRefusal is Stripe's real answer, captured in test mode
// on 2026-11-02 at API version 2025-08-27.basil (stripe-go v82's), to an
// off-session create that sets error_on_requires_action without
// payment_method_types: HTTP 400, invalid_request_error, no code and no
// PaymentIntent. Every engine charge got it until payment_method_types=card
// was sent.
const stripeDashboardMethodsRefusal = `{"error":{"message":"The ` + "`error_on_requires_action`" + ` parameter can't be used with PaymentIntents which use payment methods managed through the Dashboard at https://dashboard.stripe.com/account/payments/settings. Either omit ` + "`error_on_requires_action`" + ` when confirming this PaymentIntent (and handle any next actions that may be required) or create a different PaymentIntent that uses ` + "`payment_method_types`" + `.","type":"invalid_request_error"}}`

const stripeSucceededPI = `{"id":"pi_ok","object":"payment_intent","amount":1099,"currency":"usd","status":"succeeded","payment_method_types":["card"]}`

// stripePaymentMethodRule answers a PaymentIntent create the way Stripe does
// (https://docs.stripe.com/changelog/2023-08-16/automatic-payment-methods):
// error_on_requires_action without payment_method_types is refused, because
// the PaymentIntent then takes the Dashboard's payment methods. Otherwise the
// charge succeeds.
type stripePaymentMethodRule struct {
	forms []url.Values
}

func (s *stripePaymentMethodRule) RoundTrip(r *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, err
	}
	s.forms = append(s.forms, form)
	status, reply := http.StatusOK, stripeSucceededPI
	if form.Get("error_on_requires_action") == "true" && form.Get("payment_method_types[0]") == "" {
		status, reply = http.StatusBadRequest, stripeDashboardMethodsRefusal
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(reply)),
	}, nil
}

// Every off-session charge that asks Stripe to fail a payment needing the
// customer must also name card as its only payment method type. Without it
// Stripe refused every auto-charge, Collect payment and dunning retry with a
// 400, which Hardcount recorded as a failed payment and dunned.
func TestCreatePaymentIntent_ErrorOnRequiresActionNamesCardPaymentMethodType(t *testing.T) {
	stub := &stripePaymentMethodRule{}
	old := stripeHTTPClient.Transport
	stripeHTTPClient.Transport = stub
	t.Cleanup(func() { stripeHTTPClient.Transport = old })

	ctx := auth.WithTenantID(postgres.WithLivemode(context.Background(), true), "t1")
	c := NewLiveStripeClient(NewStripeClients(fixedCreds{}))
	res, err := c.CreatePaymentIntent(ctx, PaymentIntentParams{AmountCents: 1099, Currency: "usd", CustomerID: "cus_x", PaymentMethodID: "pm_x", IdempotencyKey: "k"})

	if len(stub.forms) != 1 {
		t.Fatalf("requests to Stripe = %d, want 1", len(stub.forms))
	}
	form := stub.forms[0]
	if form.Get("error_on_requires_action") != "true" {
		t.Fatalf("error_on_requires_action = %q; this test guards the payment method type sent with it", form.Get("error_on_requires_action"))
	}
	if got := form.Get("payment_method_types[0]"); got != "card" {
		t.Errorf("payment_method_types[0] = %q, want %q: Stripe refuses error_on_requires_action without it", got, "card")
	}
	for field := range form {
		if strings.HasPrefix(field, "payment_method_types[") && field != "payment_method_types[0]" {
			t.Errorf("sent %s=%q: Hardcount charges cards only", field, form.Get(field))
		}
		if strings.HasPrefix(field, "automatic_payment_methods") {
			t.Errorf("sent %s: Stripe refuses it with error_on_requires_action", field)
		}
	}
	if err != nil {
		t.Fatalf("CreatePaymentIntent: %v", err)
	}
	if res.ID != "pi_ok" || res.Status != "succeeded" {
		t.Errorf("result = %+v, want pi_ok succeeded", res)
	}
}

// How Stripe's 400 to the charge request itself is read today (ADR-107: a
// refusal Stripe answered is definite, never unknown). Nothing was charged and
// no PaymentIntent exists. It is not a card decline: no decline code, and no
// card message, so no customer email may show its words (Stripe writes them
// for the developer). It is not one of the refusals Hardcount names
// (the key, the minimum charge) or a transient skip, so the invoice is marked
// failed and dunned.
func TestCreatePaymentIntent_RequestRefusalIsDefiniteWithNoCardWords(t *testing.T) {
	withCannedStripe(t, http.StatusBadRequest, stripeDashboardMethodsRefusal)
	ctx := auth.WithTenantID(postgres.WithLivemode(context.Background(), true), "t1")
	c := NewLiveStripeClient(NewStripeClients(fixedCreds{}))
	_, err := c.CreatePaymentIntent(ctx, PaymentIntentParams{AmountCents: 1099, Currency: "usd", CustomerID: "cus_x", PaymentMethodID: "pm_x", IdempotencyKey: "k"})

	var pe *PaymentError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %#v, want a *PaymentError", err)
	}
	if pe.Unknown || IsUnknownPaymentFailure(err) || errors.Is(err, ErrPaymentTransient) {
		t.Fatalf("a refusal Stripe answered was read as unclear or transient: %v", err)
	}
	var ke *StripeKeyError
	var bm *BelowMinimumError
	if errors.As(err, &ke) || errors.As(err, &bm) {
		t.Fatalf("read as a key or minimum-charge refusal: %v", err)
	}
	if pe.DeclineCode != "" || pe.CardMessage != "" || pe.PaymentIntentID != "" {
		t.Errorf("DeclineCode %q CardMessage %q PaymentIntentID %q, want all empty: Stripe refused the request, not the card",
			pe.DeclineCode, pe.CardMessage, pe.PaymentIntentID)
	}
	if got := pe.OperatorSafeMessage(); strings.Contains(strings.ToLower(got), "declined") {
		t.Errorf("OperatorSafeMessage = %q: it is not a decline", got)
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
