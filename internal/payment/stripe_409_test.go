package payment

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// #976: a Stripe 409 is never a definite failure, whatever its error type.
//
// stripe-go retries a network error under the same idempotency key (v82.5.1
// shouldRetry: any non-Stripe error, up to two more requests). When the first
// request reached Stripe and is still running, the retry gets a 409. Stripe's
// advice for that answer: "If a 409 indicates that another request with the
// same key is still executing, wait and retry with that key and the same
// parameters. A new key can start a second operation."
// (https://docs.stripe.com/error-low-level). The 409 says nothing about the
// first request, which can still charge the card. Read as a definite failure,
// it marked the invoice failed and started dunning, and dunning's next retry
// sends a new key: a second charge.
//
// The responses below are decoded by stripe-go itself (through
// stripeHTTPClient), so the classifier sees exactly what a real reply
// produces.

// stripe409KeyInUse is Stripe's reply to a request whose idempotency key
// another request is still using. The 409 status
// (https://docs.stripe.com/api/errors) and the code idempotency_key_in_use
// with its message (https://docs.stripe.com/error-codes) are documented. The
// error type is not: invalid_request_error is the type the old classifier read
// as "no charge occurred". The classifier now keys on the status, so the real
// type cannot change the result. The release checklist's "Key in use" step
// gets the real reply from test mode; record its type here.
const stripe409KeyInUse = `{"error":{"code":"idempotency_key_in_use","doc_url":"https://stripe.com/docs/error-codes/idempotency-key-in-use","message":"The idempotency key provided is currently being used in another request. This occurs if your integration is making duplicate requests simultaneously.","type":"invalid_request_error"}}`

// stripe409NoCode is a 409 of type invalid_request_error with no code, so the
// rule cannot hang on the code either.
const stripe409NoCode = `{"error":{"message":"The request conflicts with another request.","type":"invalid_request_error"}}`

// stripe409CardError is a 409 of type card_error, the other type the
// classifier reads as a definite failure. It pins "whatever its type". Stripe
// sends card declines as 402, so no real 409 carries a decline code. This one
// carries a hard-decline code (lost_card, #966) to pin that a 409 never
// records one: on the unknown attempt row it would stop dunning from charging
// that card, and the API would say "Card was declined".
const stripe409CardError = `{"error":{"code":"card_declined","decline_code":"lost_card","message":"Your card was declined.","type":"card_error"}}`

// stripe409ParamsDiffer is the "same key, different parameters" answer
// (type idempotency_error). It was already an unclear outcome and keeps its
// behavior: the invoice parks (ADR-107).
const stripe409ParamsDiffer = `{"error":{"message":"Keys for idempotent requests can only be used with the same parameters they were first used with.","type":"idempotency_error"}}`

// stripe400MissingParam is a genuine rejection: Stripe refused the request
// before running it, so nothing was charged.
const stripe400MissingParam = `{"error":{"code":"parameter_missing","doc_url":"https://stripe.com/docs/error-codes/parameter-missing","message":"Missing required param: amount.","param":"amount","type":"invalid_request_error"}}`

func TestCreatePaymentIntent_A409IsNeverADefiniteFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"key in use", stripe409KeyInUse},
		{"invalid_request_error with no code", stripe409NoCode},
		{"card_error", stripe409CardError},
		{"same key, different parameters", stripe409ParamsDiffer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := withCannedStripe(t, http.StatusConflict, tc.body)
			c := NewLiveStripeClient(NewStripeClients(fixedCreds{}))

			_, err := c.CreatePaymentIntent(auth.WithTenantID(postgres.WithLivemode(context.Background(), true), "t1"), PaymentIntentParams{AmountCents: 100, Currency: "usd", CustomerID: "cus_x", PaymentMethodID: "pm_x", IdempotencyKey: "k"})
			var pe *PaymentError
			if !errors.As(err, &pe) || !pe.Unknown {
				t.Fatalf("err = %#v, want an unclear outcome (Unknown): a 409 can follow an earlier request under the same key that Stripe is still running", err)
			}
			if errors.Is(err, ErrPaymentTransient) {
				t.Errorf("a 409 was treated as 'nothing reached Stripe': %v", err)
			}
			if pe.DeclineCode != "" {
				t.Errorf("DeclineCode = %q, want none: a 409 is not a decline, and an unknown attempt row must not record one", pe.DeclineCode)
			}
			if stub.calls != 1 {
				t.Errorf("Stripe was called %d times, want 1 (stripe-go does not retry a 409 Stripe error)", stub.calls)
			}
		})
	}
}

// Negative control: the 409 rule must not widen into "invalid_request_error is
// unclear". A 400 rejection ran nothing at Stripe, and dunning should start
// at once.
func TestCreatePaymentIntent_A400RejectionStaysDefinite(t *testing.T) {
	withCannedStripe(t, http.StatusBadRequest, stripe400MissingParam)
	c := NewLiveStripeClient(NewStripeClients(fixedCreds{}))

	_, err := c.CreatePaymentIntent(auth.WithTenantID(postgres.WithLivemode(context.Background(), true), "t1"), PaymentIntentParams{AmountCents: 100, Currency: "usd", CustomerID: "cus_x", PaymentMethodID: "pm_x", IdempotencyKey: "k"})
	var pe *PaymentError
	if !errors.As(err, &pe) || pe.Unknown {
		t.Fatalf("err = %#v, want a definite failure", err)
	}
}

// scriptedStripe answers each request with the next step of a script: a
// transport error (no answer came back, though Stripe may have the request)
// or an HTTP reply. It records each request's Idempotency-Key header.
type scriptedStripe struct {
	mu    sync.Mutex
	steps []scriptedStep
	keys  []string
}

type scriptedStep struct {
	err    error
	status int
	body   string
}

func (s *scriptedStripe) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, req.Header.Get("Idempotency-Key"))
	if len(s.steps) == 0 {
		return nil, errors.New("scriptedStripe: more requests than the script has steps")
	}
	step := s.steps[0]
	s.steps = s.steps[1:]
	if step.err != nil {
		return nil, step.err
	}
	return &http.Response{
		StatusCode: step.status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(step.body)),
		Request:    req,
	}, nil
}

func withScriptedStripe(t *testing.T, steps ...scriptedStep) *scriptedStripe {
	t.Helper()
	s := &scriptedStripe{steps: steps}
	old := stripeHTTPClient.Transport
	stripeHTTPClient.Transport = s
	t.Cleanup(func() { stripeHTTPClient.Transport = old })
	return s
}

// The panel's sequence through the real charge path: the first request's
// connection drops, stripe-go retries it under the same key, and Stripe
// answers the retry with 409 "key in use" because the first request is still
// running. The charge must not count as a decline: no failed status, no
// dunning, no released hold, no new key. Until the interrupted-charge check
// ships, the invoice parks for the provider search (ADR-107/108), as any
// unclear answer with no PaymentIntent id does.
func TestChargeInvoice_NetworkErrorThenKeyInUse_IsNotADecline(t *testing.T) {
	connReset := errors.New("read tcp 10.0.0.2:51234->34.200.27.109:443: read: connection reset by peer")
	for _, tc := range []struct {
		name     string
		purpose  string
		steps    []scriptedStep
		requests int
	}{
		{
			name:     "auto-charge or Collect payment: network error, then 409 key in use",
			steps:    []scriptedStep{{err: connReset}, {status: http.StatusConflict, body: stripe409KeyInUse}},
			requests: 2,
		},
		{
			name:     "dunning retry: network error, then 409 key in use",
			purpose:  piPurposeDunningRetry,
			steps:    []scriptedStep{{err: connReset}, {status: http.StatusConflict, body: stripe409KeyInUse}},
			requests: 2,
		},
		{
			// A hard-decline code on a 409 must not reach the attempt row:
			// RetryBlockingDecline reads it whatever the outcome, and dunning
			// would stop charging the card.
			name:     "dunning retry: network error, then a 409 card_error carrying lost_card",
			purpose:  piPurposeDunningRetry,
			steps:    []scriptedStep{{err: connReset}, {status: http.StatusConflict, body: stripe409CardError}},
			requests: 2,
		},
		{
			// Control: unchanged behavior for the one 409 that keeps it.
			name:     "409 same key, different parameters",
			steps:    []scriptedStep{{status: http.StatusConflict, body: stripe409ParamsDiffer}},
			requests: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			stub := withScriptedStripe(t, tc.steps...)
			invoices := newMockInvoiceUpdater()
			snapshot := pendingSnapshot()
			invoices.invoices["inv_1"] = snapshot
			dunning := &recordingDunningStarter{}
			s := NewStripe(NewLiveStripeClient(NewStripeClients(fixedCreds{})), invoices, newMockWebhookStore(), nil, dunning)

			ctx := auth.WithTenantID(postgres.WithLivemode(context.Background(), true), "t1")
			var err error
			if tc.purpose == piPurposeDunningRetry {
				_, err = s.ChargeInvoiceForDunningRetry(ctx, "t1", snapshot, testHold, "cus_stripe", "pm_on_file")
			} else {
				_, err = s.ChargeInvoice(ctx, "t1", snapshot, testHold, "cus_stripe", "pm_on_file")
			}

			// stripe-go itself sent the retry, under the same key.
			wantKey := ChargeIdempotencyKey("inv_1", snapshot.ChargeAttemptSeq, tc.purpose) + "_pm_on_file"
			if len(stub.keys) != tc.requests {
				t.Fatalf("requests to Stripe = %d (keys %q), want %d", len(stub.keys), stub.keys, tc.requests)
			}
			for i, k := range stub.keys {
				if k != wantKey {
					t.Errorf("request %d idempotency key = %q, want %q", i+1, k, wantKey)
				}
			}

			var pe *PaymentError
			if !errors.As(err, &pe) {
				t.Fatalf("charge error = %#v, want a *PaymentError", err)
			}
			if !pe.Unknown {
				t.Errorf("charge error = %q classified as a definite failure, want an unclear outcome (Unknown)", err)
			}
			if !IsUnknownPaymentFailure(err) {
				t.Error("IsUnknownPaymentFailure = false: a dunning retry would count this as a failed attempt")
			}
			if got := pe.OperatorErrorCode(); got != "payment_outcome_unknown" {
				t.Errorf("API error code = %q, want payment_outcome_unknown", got)
			}

			if len(dunning.calls) != 0 {
				t.Errorf("StartDunning calls = %d, want 0: dunning's next retry sends a new key while the first request may still charge", len(dunning.calls))
			}
			if len(invoices.leaseReleases) != 0 {
				t.Errorf("charge hold released %d times, want 0: an unclear charge keeps the hold", len(invoices.leaseReleases))
			}
			if len(invoices.failureReports) != 0 {
				t.Errorf("failure reports = %d, want 0", len(invoices.failureReports))
			}
			got := invoices.invoices["inv_1"]
			if got.PaymentStatus != domain.PaymentUnknown || got.StripePaymentIntentID != "" {
				t.Errorf("invoice payment_status %q, PaymentIntent %q; want unknown with no PaymentIntent (parked, ADR-107), never failed",
					got.PaymentStatus, got.StripePaymentIntentID)
			}
			if len(invoices.chargeAttempts) != 1 || invoices.chargeAttempts[0].Outcome != domain.ChargeAttemptUnknown || invoices.chargeAttempts[0].DeclineCode != "" {
				t.Errorf("charge attempts = %+v, want one unknown attempt with no decline code", invoices.chargeAttempts)
			}

			// The park's ERROR names the key: a 409 proves an attempt under it
			// reached Stripe, and the operator looks the attempt up by it.
			if line := parkAnnouncement(logs.String()); line == "" {
				t.Errorf("park not announced at ERROR:\n%s", logs)
			} else if field := "idempotency_key=" + ChargeIdempotencyKey("inv_1", snapshot.ChargeAttemptSeq, tc.purpose); !slices.Contains(strings.Fields(line), field) {
				t.Errorf("park announcement does not carry %s:\n%s", field, line)
			}
		})
	}
}

// parkAnnouncement is the log line that announces a parked invoice, or "".
func parkAnnouncement(logs string) string {
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "level=ERROR") && strings.Contains(line, "this invoice is parked") {
			return line
		}
	}
	return ""
}
