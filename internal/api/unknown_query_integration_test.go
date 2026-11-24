package api

import (
	"net/http"
	"testing"
)

// TestUnknownQuery_RefusedThroughTheRouter: through the real router and
// database, a list with filters refuses a misspelled filter and answers its
// real ones, and a route that takes no query parameter refuses one and
// answers without it.
func TestUnknownQuery_RefusedThroughTheRouter(t *testing.T) {
	e := newCcyEnv(t, "Unknown Query Co")

	refused := func(r flowResp, what, param string) {
		t.Helper()
		if r.status != http.StatusUnprocessableEntity || r.errCode() != "validation_error" ||
			flowStr(r.body, "error", "param") != param || r.errMessage() != `unknown query parameter "`+param+`"` {
			t.Errorf("%s: %d %s, want 422 validation_error naming %s", what, r.status, r.raw, param)
		}
	}

	refused(e.call(t, "GET", "/v1/invoices?staus=paid", nil), "GET /v1/invoices?staus=paid", "staus")
	e.call(t, "GET", "/v1/invoices?status=paid&customer_id=&limit=5", nil).want(t, http.StatusOK, "GET /v1/invoices with its own filters")

	cus := e.customer(t, "unknown-query")
	refused(e.call(t, "GET", "/v1/customers/"+cus+"?expand=subscriptions", nil), "GET /v1/customers/{id}?expand=subscriptions", "expand")
	e.call(t, "GET", "/v1/customers/"+cus, nil).want(t, http.StatusOK, "GET /v1/customers/{id}")
}
