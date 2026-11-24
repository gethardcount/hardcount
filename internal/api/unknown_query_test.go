package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/request"
)

// queryCheckAnswer serves method and target through refuseUnknownQuery, on a
// record whose route is pattern, a route table entry. It returns the status,
// and the error's param and message when refused; a request that passes
// reaches a handler that answers 204.
func queryCheckAnswer(t *testing.T, method, pattern, target string) (status int, param, message string) {
	t.Helper()
	route := func(r *http.Request) request.Route { return request.Route{Method: r.Method, Pattern: pattern} }
	if _, ok := routeTable[routeKey{Method: method, Pattern: pattern}]; !ok {
		t.Fatalf("%s %s is not in the route table", method, pattern)
	}
	h := request.Begin(route)(refuseUnknownQuery(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	if rec.Code == http.StatusNoContent {
		return rec.Code, "", ""
	}
	var body struct {
		Error struct{ Code, Param, Message string }
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("%s %s: decode %d answer: %v", method, target, rec.Code, err)
	}
	if body.Error.Code != "validation_error" {
		t.Errorf("%s %s: code %q, want validation_error", method, target, body.Error.Code)
	}
	return rec.Code, body.Error.Param, body.Error.Message
}

// A name the route does not take is refused: 422 validation_error, param
// naming it, and a message that says it is an unknown query parameter.
func TestRefuseUnknownQuery_UnknownNameIs422(t *testing.T) {
	status, param, msg := queryCheckAnswer(t, "GET", "/v1/invoices", "/v1/invoices?staus=paid")
	if status != http.StatusUnprocessableEntity || param != "staus" || msg != `unknown query parameter "staus"` {
		t.Errorf("GET /v1/invoices?staus=paid = %d param %q message %q, want 422 param staus message %q",
			status, param, msg, `unknown query parameter "staus"`)
	}
}

// Names the route takes pass, an empty value and the paging pair included.
func TestRefuseUnknownQuery_KnownNamesPass(t *testing.T) {
	for _, target := range []string{
		"/v1/invoices",
		"/v1/invoices?status=paid&customer_id=hc_cus_1&limit=10&offset=20",
		"/v1/invoices?status=",
	} {
		if status, param, _ := queryCheckAnswer(t, "GET", "/v1/invoices", target); status != http.StatusNoContent {
			t.Errorf("GET %s = %d (param %q), want it to reach the handler", target, status, param)
		}
	}
}

// With two unknown names, the answer names the first in sorted order, so the
// same request always gets the same answer.
func TestRefuseUnknownQuery_TwoUnknownNamesNameTheFirstSorted(t *testing.T) {
	if _, param, _ := queryCheckAnswer(t, "GET", "/v1/invoices", "/v1/invoices?zeta=1&status=paid&beta=2"); param != "beta" {
		t.Errorf("param %q, want beta", param)
	}
}

// A route whose entry declares no query parameters takes none, whatever its
// method.
func TestRefuseUnknownQuery_RouteWithNoDeclarationRefusesAnyName(t *testing.T) {
	for _, c := range []struct{ method, pattern, target, param string }{
		{"GET", "/v1/invoices/{id}", "/v1/invoices/hc_inv_1?expand=lines", "expand"},
		{"POST", "/v1/invoices/{id}/void", "/v1/invoices/hc_inv_1/void?limit=1", "limit"},
	} {
		status, param, _ := queryCheckAnswer(t, c.method, c.pattern, c.target)
		if status != http.StatusUnprocessableEntity || param != c.param {
			t.Errorf("%s %s = %d param %q, want 422 param %s", c.method, c.target, status, param, c.param)
		}
	}
}

// A query string Go cannot parse is refused whole, with no param: its lenient
// reading drops the pair, and a dropped filter answers as if none was sent.
func TestRefuseUnknownQuery_QueryThatDoesNotParseIs422(t *testing.T) {
	for _, query := range []string{"staus=paid;", "status=paid;limit=5", "status=%zz"} {
		status, param, message := queryCheckAnswer(t, "GET", "/v1/invoices", "/v1/invoices?"+query)
		if status != http.StatusUnprocessableEntity || param != "" || message == "" {
			t.Errorf("GET /v1/invoices?%s = %d, param %q, message %q; want 422 with a message and no param", query, status, param, message)
		}
	}
}

// A route marked withAnyQuery takes any name.
func TestRefuseUnknownQuery_AnyQueryRoutePassesAnyName(t *testing.T) {
	if status, param, _ := queryCheckAnswer(t, "POST", "/v1/webhooks/postmark", "/v1/webhooks/postmark?anything=1&else="); status != http.StatusNoContent {
		t.Errorf("POST /v1/webhooks/postmark?anything=1 = %d (param %q), want it to reach the handler", status, param)
	}
}

// Through the real router, a request with a key that is not valid and an
// unknown name answers 401: the check runs after authentication, so it never
// tells a caller who is not signed in that a route exists.
func TestRefuseUnknownQuery_BadKeyIs401First(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/invoices?staus=paid", nil)
	req.Header.Set("Authorization", "Bearer not-a-key")
	rec := httptest.NewRecorder()
	newTestServer().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /v1/invoices?staus=paid with a bad key = %d %s, want 401", rec.Code, rec.Body.String())
	}
}
