package payment

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gethardcount/hardcount/internal/errs"
)

// TestAppendStatusQuery pins the success/cancel return-URL builder for the
// public payment-update Stripe Checkout flow: success and cancel must land on
// the SAME public page with a distinguishing ?status=, and a base that already
// carries a query string must use & rather than a second ?.
func TestAppendStatusQuery(t *testing.T) {
	cases := []struct {
		name   string
		base   string
		status string
		want   string
	}{
		{
			name:   "no existing query → ?status",
			base:   "http://localhost:5173/payment-method-added",
			status: "success",
			want:   "http://localhost:5173/payment-method-added?status=success",
		},
		{
			name:   "cancel variant",
			base:   "http://localhost:5173/payment-method-added",
			status: "cancel",
			want:   "http://localhost:5173/payment-method-added?status=cancel",
		},
		{
			name:   "existing query → &status",
			base:   "https://app.example.com/account?ref=email",
			status: "success",
			want:   "https://app.example.com/account?ref=email&status=success",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := appendStatusQuery(tc.base, tc.status); got != tc.want {
				t.Errorf("appendStatusQuery(%q, %q) = %q, want %q", tc.base, tc.status, got, tc.want)
			}
		})
	}
}

// TestCustomerReadFailed_NotFoundIs404: a payment-update link whose customer
// is gone answers 404 customer_not_found. The customer store reports a
// missing row as errs.ErrNotFound (customer.PostgresStore.Get maps
// sql.ErrNoRows to it), and the handler used to test for sql.ErrNoRows, so
// the case answered 500. Any other read error stays a 500.
// Mutation-verify: test for sql.ErrNoRows again → the not-found cases get 500.
func TestCustomerReadFailed_NotFoundIs404(t *testing.T) {
	for _, c := range []struct {
		name     string
		err      error
		wantCode int
		wantErr  string
	}{
		{"the store's not-found", errs.ErrNotFound, http.StatusNotFound, "customer_not_found"},
		{"a wrapped not-found", fmt.Errorf("customer: %w", errs.ErrNotFound), http.StatusNotFound, "customer_not_found"},
		{"a read that failed", errors.New("dial tcp: connection refused"), http.StatusInternalServerError, "internal_error"},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/public/payment-updates/tok", nil)
			rec := httptest.NewRecorder()
			customerReadFailed(req.Context(), rec, req, "hc_cus_gone", c.err)
			var env struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode %s: %v", rec.Body.String(), err)
			}
			if rec.Code != c.wantCode || env.Error.Code != c.wantErr {
				t.Fatalf("answer %d %q, want %d %q; body %s", rec.Code, env.Error.Code, c.wantCode, c.wantErr, rec.Body.String())
			}
		})
	}
}
