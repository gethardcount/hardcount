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
func TestConfirmPaymentIntent_PaymentNeedingTheCustomerFailsAsConfirmDecline(t *testing.T) {
	stub := &capturingStripe{cannedStripe: cannedStripe{status: http.StatusPaymentRequired, body: stripeAuthNotHandled}}
	old := stripeHTTPClient.Transport
	stripeHTTPClient.Transport = stub
	t.Cleanup(func() { stripeHTTPClient.Transport = old })

	ctx := auth.WithTenantID(postgres.WithLivemode(context.Background(), true), "t1")
	c := NewLiveStripeClient(NewStripeClients(fixedCreds{}))
	_, err := c.ConfirmPaymentIntent(ctx, "pi_1G8JtxDpqHItWkFAnB32FhtI", PaymentIntentParams{AmountCents: 1099, Currency: "usd", CustomerID: "cus_x", PaymentMethodID: "pm_x", IdempotencyKey: "k"})

	if len(stub.forms) != 1 {
		t.Fatalf("requests to Stripe = %d, want 1", len(stub.forms))
	}
	for field, want := range map[string]string{"off_session": "true", "error_on_requires_action": "true", "payment_method": "pm_x"} {
		if got := stub.forms[0].Get(field); got != want {
			t.Errorf("PaymentIntent confirm sent %s=%q, want %q", field, got, want)
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

const stripeUnconfirmedPI = `{"id":"pi_ok","object":"payment_intent","amount":1099,"currency":"usd","status":"requires_confirmation","payment_method_types":["card"]}`

// stripePaymentMethodRule answers a charge's two calls the way Stripe does
// (https://docs.stripe.com/changelog/2023-08-16/automatic-payment-methods): a
// confirm with error_on_requires_action is refused unless the PaymentIntent
// was created with payment_method_types (otherwise it takes the Dashboard's
// payment methods). Checked against Stripe test mode on 2026-11-09: a
// PaymentIntent created with payment_method_types[]=card and confirmed with
// error_on_requires_action=true succeeds.
type stripePaymentMethodRule struct {
	forms    []url.Values
	paths    []string
	cardOnly bool // the created PaymentIntent named card as its only type
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
	s.paths = append(s.paths, r.URL.Path)
	status, reply := http.StatusOK, stripeUnconfirmedPI
	if strings.HasSuffix(r.URL.Path, "/confirm") {
		reply = stripeSucceededPI
		if form.Get("error_on_requires_action") == "true" && !s.cardOnly {
			status, reply = http.StatusBadRequest, stripeDashboardMethodsRefusal
		}
	} else {
		s.cardOnly = form.Get("payment_method_types[0]") == "card"
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
// 400, which Hardcount recorded as a failed payment and dunned. The type goes
// on the create; error_on_requires_action and off_session go on the confirm,
// the only call Stripe takes them with.
func TestChargeCalls_ErrorOnRequiresActionNamesCardPaymentMethodType(t *testing.T) {
	stub := &stripePaymentMethodRule{}
	old := stripeHTTPClient.Transport
	stripeHTTPClient.Transport = stub
	t.Cleanup(func() { stripeHTTPClient.Transport = old })

	ctx := auth.WithTenantID(postgres.WithLivemode(context.Background(), true), "t1")
	c := NewLiveStripeClient(NewStripeClients(fixedCreds{}))
	p := PaymentIntentParams{AmountCents: 1099, Currency: "usd", CustomerID: "cus_x", PaymentMethodID: "pm_x", IdempotencyKey: "k"}
	created, err := c.CreateUnconfirmedPaymentIntent(ctx, p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err := c.ConfirmPaymentIntent(ctx, created.ID, p)

	if len(stub.forms) != 2 {
		t.Fatalf("requests to Stripe = %d, want 2 (create, confirm)", len(stub.forms))
	}
	create, confirm := stub.forms[0], stub.forms[1]
	if got := create.Get("payment_method_types[0]"); got != "card" {
		t.Errorf("create sent payment_method_types[0] = %q, want %q: Stripe refuses error_on_requires_action without it", got, "card")
	}
	for _, field := range []string{"confirm", "off_session", "error_on_requires_action"} {
		if create.Has(field) {
			t.Errorf("create sent %s=%q: the create must move no money and leave confirm-time settings to the confirm", field, create.Get(field))
		}
	}
	for field := range create {
		if strings.HasPrefix(field, "payment_method_types[") && field != "payment_method_types[0]" {
			t.Errorf("create sent %s=%q: Hardcount charges cards only", field, create.Get(field))
		}
		if strings.HasPrefix(field, "automatic_payment_methods") {
			t.Errorf("create sent %s: Stripe refuses it with error_on_requires_action", field)
		}
	}
	if !strings.HasSuffix(stub.paths[1], "/v1/payment_intents/pi_ok/confirm") {
		t.Errorf("second request went to %s, want the created PaymentIntent's confirm", stub.paths[1])
	}
	for field, want := range map[string]string{"error_on_requires_action": "true", "off_session": "true", "payment_method": "pm_x"} {
		if got := confirm.Get(field); got != want {
			t.Errorf("confirm sent %s=%q, want %q", field, got, want)
		}
	}
	if err != nil {
		t.Fatalf("confirm: %v", err)
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
	_, err := c.CreateUnconfirmedPaymentIntent(ctx, PaymentIntentParams{AmountCents: 1099, Currency: "usd", CustomerID: "cus_x", PaymentMethodID: "pm_x", IdempotencyKey: "k"})

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
