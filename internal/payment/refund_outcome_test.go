package payment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/auth"
)

// Stripe error bodies, shaped like Stripe's error object
// (https://docs.stripe.com/api/errors). Stripe says a 5xx result is
// indeterminate: "the operation might have produced side effects even though
// you received an error" (https://docs.stripe.com/error-low-level#server-errors).
const (
	stripe500             = `{"error":{"message":"An unknown error occurred","type":"api_error"}}`
	stripeAlreadyRefunded = `{"error":{"code":"charge_already_refunded","message":"Charge ch_1 has already been refunded.","type":"invalid_request_error"}}`
)

// refundOutcome is how the credit-note service reads a refund error. It is
// declared here, not imported, because creditnote may not import payment.
type refundOutcome interface{ OutcomeUnknown() bool }

// noAnswer fails every request at the transport, like a timeout or a dropped
// connection: Velox never sees Stripe's answer.
type noAnswer struct{ calls int }

func (n *noAnswer) RoundTrip(*http.Request) (*http.Response, error) {
	n.calls++
	return nil, errors.New("read tcp 10.0.0.1:443: i/o timeout")
}

// TestCreateRefund_SaysWhetherTheRefundMayExist pins #965. CreateRefund used
// to flatten every Stripe error into a string, so the credit note recorded a
// timeout or a Stripe 5xx as "refund failed", although Stripe may have made
// the refund. Someone who trusts that label and refunds again in the Stripe
// Dashboard pays the customer twice.
//
// Each case goes through the real stripe-go client, so the test reads the
// error stripe-go builds from the response, not one written by hand.
func TestCreateRefund_SaysWhetherTheRefundMayExist(t *testing.T) {
	cases := []struct {
		name    string
		stripe  func(t *testing.T)
		unknown bool
		code    string // the 502 code the API answers with
	}{
		{
			name:    "Stripe 500: the refund may exist",
			stripe:  func(t *testing.T) { withCannedStripe(t, http.StatusInternalServerError, stripe500) },
			unknown: true,
			code:    "payment_outcome_unknown",
		},
		{
			name: "no answer: the request may have reached Stripe",
			stripe: func(t *testing.T) {
				old := stripeHTTPClient.Transport
				stripeHTTPClient.Transport = &noAnswer{}
				t.Cleanup(func() { stripeHTTPClient.Transport = old })
			},
			unknown: true,
			code:    "payment_outcome_unknown",
		},
		{
			name:    "invalid request: Stripe refused, nothing was refunded",
			stripe:  func(t *testing.T) { withCannedStripe(t, http.StatusBadRequest, stripeAlreadyRefunded) },
			unknown: false,
			code:    "provider_error",
		},
		{
			// Stripe sends 429 before it runs a request, and saves no result
			// for it (https://docs.stripe.com/error-low-level#content-errors).
			// stripe429 is a real test-mode reply; its type is
			// rate_limit_error, which the charge classifier alone reads as
			// unclear.
			name:    "rate limit: Stripe refused before running the request",
			stripe:  func(t *testing.T) { withCannedStripe(t, http.StatusTooManyRequests, stripe429) },
			unknown: false,
			code:    "provider_error",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.stripe(t)
			r := NewStripeRefunder(NewStripeClients(fixedCreds{}))
			ctx := auth.WithTenantID(context.Background(), "t1")

			_, _, err := r.CreateRefund(ctx, "pi_x", 500, "velox_cn_x", "cn_x")
			if err == nil {
				t.Fatal("CreateRefund returned no error")
			}
			var oe refundOutcome
			if !errors.As(err, &oe) {
				t.Fatalf("CreateRefund error %T (%v) does not say whether Stripe may have made the refund", err, err)
			}
			if oe.OutcomeUnknown() != tc.unknown {
				t.Fatalf("OutcomeUnknown() = %v, want %v (error: %v)", oe.OutcomeUnknown(), tc.unknown, err)
			}

			// What an API caller of retry-refund sees: a 502 with a code
			// that tells "no clear answer" from "Stripe refused".
			rr := httptest.NewRecorder()
			respond.FromError(rr, httptest.NewRequest(http.MethodPost, "/v1/credit-notes/cn_x/retry-refund", nil), err, "credit_note")
			var body respond.ErrorBody
			if jerr := json.Unmarshal(rr.Body.Bytes(), &body); jerr != nil {
				t.Fatalf("decode %q: %v", rr.Body.String(), jerr)
			}
			if rr.Code != http.StatusBadGateway || body.Error.Code != tc.code {
				t.Fatalf("API answer = %d %q, want 502 %q (body %s)", rr.Code, body.Error.Code, tc.code, rr.Body.String())
			}
		})
	}
}
