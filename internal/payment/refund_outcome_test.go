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
	// Stripe's answer while another request with the same idempotency key
	// is still running (https://docs.stripe.com/error-codes, code
	// idempotency_key_in_use). Its type reads as a refusal.
	stripeRefundKeyInUse = `{"error":{"code":"idempotency_key_in_use","message":"The idempotency key provided is currently being used in another request.","type":"invalid_request_error"}}`
	// Stripe's answer to a revoked or mistyped secret key.
	stripeBadKey = `{"error":{"message":"Invalid API Key provided: sk_test_****FAKE","type":"invalid_request_error"}}`
	// Stripe's answer to a restricted key (rk_, which Velox accepts) that
	// lacks refund permission: 403, "The API key doesn't have permissions to
	// perform the request" (https://docs.stripe.com/api/errors).
	stripeKeyNoPermission = `{"error":{"message":"The provided key 'rk_test_****FAKE' does not have the required permissions for this endpoint on account 'acct_FAKE'.","type":"invalid_request_error"}}`
)

// refundOutcome is how the credit-note service reads a refund error. It is
// declared here, not imported, because creditnote may not import payment.
type refundOutcome interface {
	OutcomeUnknown() bool
	RequestNotRun() bool
}

// noAnswer fails every request at the transport, like a timeout or a dropped
// connection: Velox never sees Stripe's answer.
type noAnswer struct{ calls int }

func (n *noAnswer) RoundTrip(*http.Request) (*http.Response, error) {
	n.calls++
	return nil, errors.New("read tcp 10.0.0.1:443: i/o timeout")
}

// lostThenCanned loses the first request on the wire, then answers every
// later one with a fixed response. stripe-go resends a request lost on the
// wire, under the same idempotency key, and stops at any answer from Stripe
// but a 429 lock_timeout (stripe-go v82 stripe.go shouldRetry, error.go
// canRetry). So the error Velox reads can be the answer to a second attempt
// while the first one ran.
type lostThenCanned struct {
	status int
	body   string
	calls  int
}

func (l *lostThenCanned) RoundTrip(r *http.Request) (*http.Response, error) {
	l.calls++
	if l.calls == 1 {
		return nil, errors.New("read tcp 10.0.0.1:443: connection reset by peer")
	}
	return (&cannedStripe{status: l.status, body: l.body}).RoundTrip(r)
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
		name      string
		stripe    func(t *testing.T)
		wantCalls int // requests stripe-go sends; 0 skips the check
		unknown   bool
		notRun    bool   // Stripe ran nothing, so it saved no result for the key
		code      string // the 502 code the API answers with
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
			// Retry refund resends an unclear request under its own key. If
			// that request is still running, this is the answer, and the
			// refund may still be made.
			name:    "409 key in use: the first request may still make the refund",
			stripe:  func(t *testing.T) { withCannedStripe(t, http.StatusConflict, stripeRefundKeyInUse) },
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
			// But stripe-go may have sent the request before, and lost it on
			// the wire (next case), and Velox cannot tell. stripe429 is a
			// real test-mode reply.
			name:    "rate limit: an earlier attempt may have run",
			stripe:  func(t *testing.T) { withCannedStripe(t, http.StatusTooManyRequests, stripe429) },
			unknown: true,
			code:    "payment_outcome_unknown",
		},
		{
			// The first attempt reached Stripe or not; the second got a 429.
			// Recording "failed" here would free the next retry to use a new
			// key while Stripe may be making the first refund.
			name: "connection lost, then a rate limit: the first attempt may have run",
			stripe: func(t *testing.T) {
				withTransport(t, &lostThenCanned{status: http.StatusTooManyRequests, body: stripe429})
			},
			wantCalls: 2,
			unknown:   true,
			code:      "payment_outcome_unknown",
		},
		{
			// An invalid API key is refused before execution, with no saved
			// result (same page).
			name:    "invalid API key: Stripe refused before running the request",
			stripe:  func(t *testing.T) { withCannedStripe(t, http.StatusUnauthorized, stripeBadKey) },
			unknown: false,
			notRun:  true,
			code:    "provider_error",
		},
		{
			// Every attempt of one call carries the same API key, so if the
			// last was refused for the key, any earlier one that reached
			// Stripe was refused the same way, before it ran.
			name: "connection lost, then an invalid API key: no attempt ran",
			stripe: func(t *testing.T) {
				withTransport(t, &lostThenCanned{status: http.StatusUnauthorized, body: stripeBadKey})
			},
			wantCalls: 2,
			unknown:   false,
			notRun:    true,
			code:      "provider_error",
		},
		{
			// A key without refund permission is also refused before the
			// request runs. On a resend, this answer says nothing about the
			// earlier request: the tenant may have changed keys since.
			name:    "key without refund permission: Stripe refused before running the request",
			stripe:  func(t *testing.T) { withCannedStripe(t, http.StatusForbidden, stripeKeyNoPermission) },
			unknown: false,
			notRun:  true,
			code:    "provider_error",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.stripe(t)
			sent := countRequests(t)
			r := NewStripeRefunder(NewStripeClients(fixedCreds{}))
			ctx := auth.WithTenantID(context.Background(), "t1")

			_, _, err := r.CreateRefund(ctx, "pi_x", 500, "velox_cn_x", "cn_x")
			if err == nil {
				t.Fatal("CreateRefund returned no error")
			}
			if tc.wantCalls != 0 && *sent != tc.wantCalls {
				t.Fatalf("requests sent = %d, want %d", *sent, tc.wantCalls)
			}
			var oe refundOutcome
			if !errors.As(err, &oe) {
				t.Fatalf("CreateRefund error %T (%v) does not say whether Stripe may have made the refund", err, err)
			}
			if oe.OutcomeUnknown() != tc.unknown {
				t.Fatalf("OutcomeUnknown() = %v, want %v (error: %v)", oe.OutcomeUnknown(), tc.unknown, err)
			}
			if oe.RequestNotRun() != tc.notRun {
				t.Fatalf("RequestNotRun() = %v, want %v (error: %v)", oe.RequestNotRun(), tc.notRun, err)
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

// withTransport answers every Stripe request through rt for this test.
func withTransport(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	old := stripeHTTPClient.Transport
	stripeHTTPClient.Transport = rt
	t.Cleanup(func() { stripeHTTPClient.Transport = old })
}

// countRequests counts the requests that reach the transport set for this
// test.
func countRequests(t *testing.T) *int {
	t.Helper()
	n := new(int)
	next := stripeHTTPClient.Transport
	stripeHTTPClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		*n++
		return next.RoundTrip(r)
	})
	t.Cleanup(func() { stripeHTTPClient.Transport = next })
	return n
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
