package tenantstripe

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stripe/stripe-go/v82"
)

// The dashboard used to show stripe-go's Error() verbatim, which is the
// whole error marshalled as JSON. describeVerifyError keeps only Stripe's
// sentence and says whether Stripe refused the key or was never asked.
func TestDescribeVerifyError(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantKind string
		wantMsg  string
	}{
		{
			name:     "invalid key: Stripe refused it",
			err:      &stripe.Error{HTTPStatusCode: 401, Type: stripe.ErrorTypeInvalidRequest, Msg: "Invalid API Key provided: sk_test_****abcd"},
			wantKind: VerifyErrorRejected,
			wantMsg:  "Invalid API Key provided: sk_test_****abcd",
		},
		{
			name:     "restricted key without access: refused",
			err:      fmt.Errorf("wrapped: %w", &stripe.Error{HTTPStatusCode: 403, Msg: "The provided key does not have the required permissions."}),
			wantKind: VerifyErrorRejected,
			wantMsg:  "The provided key does not have the required permissions.",
		},
		{
			name:     "rate limited: not checked",
			err:      &stripe.Error{HTTPStatusCode: 429, Msg: "Too many requests."},
			wantKind: VerifyErrorUnavailable,
			wantMsg:  "Too many requests.",
		},
		{
			name:     "Stripe outage: not checked",
			err:      &stripe.Error{HTTPStatusCode: 503, Type: stripe.ErrorTypeAPI},
			wantKind: VerifyErrorUnavailable,
			wantMsg:  "Stripe answered with HTTP 503.",
		},
		{
			name:     "no answer at all: not checked",
			err:      fmt.Errorf("Get \"https://api.stripe.com/v1/account\": %w", context.DeadlineExceeded),
			wantKind: VerifyErrorUnavailable,
			wantMsg:  "Get \"https://api.stripe.com/v1/account\": context deadline exceeded",
		},
		{
			name:     "dial failure: not checked",
			err:      errors.New("dial tcp: lookup api.stripe.com: no such host"),
			wantKind: VerifyErrorUnavailable,
			wantMsg:  "dial tcp: lookup api.stripe.com: no such host",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, msg := describeVerifyError(c.err)
			if kind != c.wantKind || msg != c.wantMsg {
				t.Errorf("describeVerifyError = (%q, %q), want (%q, %q)", kind, msg, c.wantKind, c.wantMsg)
			}
		})
	}
}
