package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestListFilters_UnknownValueIs422: a list filter takes a fixed set of
// values. Each value in the set answers 200, and any other value answers 422
// validation_error with `param` naming the filter and the message listing the
// set. Passed on to the store, an unknown value matched no row, and the empty
// 200 read as "none of those".
func TestListFilters_UnknownValueIs422(t *testing.T) {
	e := newCcyEnv(t, "List Filter Values Co")
	cus := e.customer(t, "filter-values")

	for _, tc := range []struct {
		path, param string
		allowed     []string
	}{
		{"/v1/invoices", "status", []string{"draft", "finalized", "paid", "voided", "uncollectible"}},
		{"/v1/invoices", "payment_status", []string{"pending", "processing", "succeeded", "failed", "unknown"}},
		{"/v1/credit-notes", "status", []string{"draft", "issued", "voided"}},
		{"/v1/credit-notes", "refund_status", []string{"none", "pending", "succeeded", "failed", "unknown", "needs_attention"}},
		{"/v1/customers", "status", []string{"active", "archived"}},
		{"/v1/credits/ledger/" + cus, "entry_type", []string{"grant", "usage", "expiry", "adjustment"}},
	} {
		for _, v := range tc.allowed {
			if r := e.call(t, "GET", tc.path+"?"+tc.param+"="+v, nil); r.status != http.StatusOK {
				t.Errorf("GET %s?%s=%s: status %d, want 200; body: %s", tc.path, tc.param, v, r.status, r.raw)
			}
		}
		for _, bad := range []string{"bogus", strings.ToUpper(tc.allowed[0])} {
			r := e.call(t, "GET", tc.path+"?"+tc.param+"="+bad, nil)
			if r.status != http.StatusUnprocessableEntity {
				t.Errorf("GET %s?%s=%s: status %d, want 422; body: %s", tc.path, tc.param, bad, r.status, r.raw)
				continue
			}
			if code, param := r.errCode(), flowStr(r.body, "error", "param"); code != "validation_error" || param != tc.param {
				t.Errorf("GET %s?%s=%s: code %q param %q, want validation_error on %s", tc.path, tc.param, bad, code, param, tc.param)
			}
			if want := strings.Join(tc.allowed, ", "); !strings.Contains(r.errMessage(), want) {
				t.Errorf("GET %s?%s=%s: message %q does not list %s", tc.path, tc.param, bad, r.errMessage(), want)
			}
		}
	}
}
