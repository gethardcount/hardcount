package payment

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
)

// The stale-hold sweep's read (ADR-127), decoded by stripe-go itself through
// stripeHTTPClient, so the code sees exactly what a real reply produces.
// Ported from PR #1291's list test.

// routedStripe answers by request path. Each route returns (status, body).
type routedStripe struct {
	mu       sync.Mutex
	routes   map[string]func(req *http.Request) (int, string)
	requests []*http.Request
}

func (r *routedStripe) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.requests = append(r.requests, req)
	route, ok := r.routes[req.Method+" "+req.URL.Path]
	r.mu.Unlock()
	status, body := http.StatusNotFound, `{"error":{"type":"invalid_request_error","message":"Unrecognized request URL"}}`
	if ok {
		status, body = route(req)
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

func withRoutedStripe(t *testing.T, routes map[string]func(req *http.Request) (int, string)) *routedStripe {
	t.Helper()
	r := &routedStripe{routes: routes}
	old := stripeHTTPClient.Transport
	stripeHTTPClient.Transport = r
	t.Cleanup(func() { stripeHTTPClient.Transport = old })
	return r
}

const (
	listCustomerMissing = `{"error":{"code":"resource_missing","message":"No such customer: 'cus_x'","param":"customer","type":"invalid_request_error"}}`
	listBadKey          = `{"error":{"message":"Invalid API Key provided: sk_test_****","type":"invalid_request_error"}}`
	listPage500         = `{"error":{"message":"An unknown error occurred","type":"api_error"}}`
	listNoPermission    = `{"error":{"message":"The provided key 'rk_test_****' does not have the required permissions for this endpoint on account 'acct_x'. Having the 'rak_payment_intent_read' permission would allow this request to continue.","type":"invalid_request_error"}}`

	piPage1 = `{"object":"list","url":"/v1/payment_intents","has_more":true,"data":[
		{"id":"pi_a","object":"payment_intent","status":"succeeded","created":1760000100,"amount_received":5000,
		 "metadata":{"velox_invoice_id":"inv_1"},
		 "latest_charge":{"id":"ch_a","object":"charge","refunded":false}},
		{"id":"pi_b","object":"payment_intent","status":"succeeded","created":1760000050,"amount_received":5000,
		 "metadata":{"velox_invoice_id":"inv_1","velox_purpose":"dunning_retry"},
		 "latest_charge":{"id":"ch_b","object":"charge","refunded":true}}]}`
	piPage2 = `{"object":"list","url":"/v1/payment_intents","has_more":false,"data":[
		{"id":"pi_c","object":"payment_intent","status":"processing","created":1760000010,"amount_received":0,
		 "metadata":{"velox_invoice_id":"inv_2","velox_purpose":"hosted_invoice_pay"},"latest_charge":null}]}`
)

func listClient() (*LiveStripeClient, context.Context) {
	return NewLiveStripeClient(NewStripeClients(fixedCreds{})), auth.WithTenantID(context.Background(), "t1")
}

// Every page is read; each PaymentIntent carries its invoice, purpose,
// created time, captured amount and whether its latest charge was refunded in
// full; the request asks for this customer from the given time on, 100 a
// page, with latest_charge expanded.
func TestListPaymentIntentsForCustomer_ReadsEveryPage(t *testing.T) {
	stub := withRoutedStripe(t, map[string]func(*http.Request) (int, string){
		"GET /v1/payment_intents": func(req *http.Request) (int, string) {
			if req.URL.Query().Get("starting_after") == "" {
				return http.StatusOK, piPage1
			}
			return http.StatusOK, piPage2
		},
	})
	c, ctx := listClient()
	since := time.Unix(1760000000, 0).UTC()

	got, err := c.ListPaymentIntentsForCustomer(ctx, "cus_x", since)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d PaymentIntents, want 3 across two pages: %+v", len(got), got)
	}
	a, b, c3 := got[0], got[1], got[2]
	if a.ID != "pi_a" || a.Status != domain.ProviderPISucceeded || a.InvoiceID != "inv_1" || a.AmountReceivedCents != 5000 || a.CreatedAt != 1760000100 || a.FullyRefunded {
		t.Errorf("pi_a = %+v", a)
	}
	if b.Purpose != "dunning_retry" || !b.FullyRefunded {
		t.Errorf("pi_b = %+v, want its purpose and FullyRefunded (latest_charge.refunded)", b)
	}
	if c3.ID != "pi_c" || c3.InvoiceID != "inv_2" || c3.Purpose != PurposeHostedInvoicePay || c3.FullyRefunded {
		t.Errorf("pi_c = %+v", c3)
	}

	if len(stub.requests) != 2 {
		t.Fatalf("list requests = %d, want 2 (one per page)", len(stub.requests))
	}
	q := stub.requests[0].URL.Query()
	if q.Get("customer") != "cus_x" || q.Get("created[gte]") != "1760000000" || q.Get("limit") != "100" {
		t.Errorf("list query = %v, want customer=cus_x, created[gte]=1760000000, limit=100", q)
	}
	if exp := q["expand[0]"]; len(exp) != 1 || exp[0] != "data.latest_charge" {
		t.Errorf("list query expand = %v, want data.latest_charge (the refund check reads it)", q)
	}
}

// A failed read is never "no such charge": an error on any page fails the
// whole read and returns nothing, and the two refusals that will not heal on
// retry are typed for the sweep.
func TestListPaymentIntentsForCustomer_AFailedReadIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		routes map[string]func(*http.Request) (int, string)
		want   error // nil: any error that is neither typed refusal
	}{
		{"error on the second page", map[string]func(*http.Request) (int, string){
			"GET /v1/payment_intents": func(req *http.Request) (int, string) {
				if req.URL.Query().Get("starting_after") == "" {
					return http.StatusOK, piPage1
				}
				return http.StatusInternalServerError, listPage500
			},
		}, nil},
		{"customer missing in this account", map[string]func(*http.Request) (int, string){
			"GET /v1/payment_intents": func(*http.Request) (int, string) { return http.StatusNotFound, listCustomerMissing },
		}, ErrStripeCustomerMissing},
		{"API key rejected", map[string]func(*http.Request) (int, string){
			"GET /v1/payment_intents": func(*http.Request) (int, string) { return http.StatusUnauthorized, listBadKey },
		}, ErrStripeAuthFailed},
		// A restricted key without PaymentIntent read access. Retried as a
		// passing failure, it logged a WARN every tick and never raised the
		// error that points the operator to the runbook.
		{"API key lacks permission (403)", map[string]func(*http.Request) (int, string){
			"GET /v1/payment_intents": func(*http.Request) (int, string) { return http.StatusForbidden, listNoPermission },
		}, ErrStripeAuthFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withRoutedStripe(t, tc.routes)
			c, ctx := listClient()
			got, err := c.ListPaymentIntentsForCustomer(ctx, "cus_x", time.Unix(1760000000, 0))
			if err == nil {
				t.Fatalf("got %d PaymentIntents and no error: a failed read must never read as an answer", len(got))
			}
			if got != nil {
				t.Errorf("a failed read returned %d PaymentIntents alongside its error", len(got))
			}
			switch {
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Errorf("err = %v, want %v", err, tc.want)
			case tc.want == nil && (errors.Is(err, ErrStripeCustomerMissing) || errors.Is(err, ErrStripeAuthFailed) || errors.Is(err, ErrStripeNotConfigured)):
				t.Errorf("err = %v: a retryable failure was typed as a refusal that will not heal", err)
			}
		})
	}
}

// The charge's re-check right before sending (ADR-127): when the caller's
// hold is no longer its own, or the invoice changed, nothing is sent, the hold
// is released, and the charge reports a transient skip.
func TestChargeInvoice_RecheckRefused_SendsNothing(t *testing.T) {
	client := &mockStripeClient{}
	invoices := newMockInvoiceUpdater()
	invoices.confirmRefused = true
	invoices.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", InvoiceNumber: "INV-1", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentPending, AmountDueCents: 5000, Currency: "USD",
	}
	s := NewStripe(client, invoices, newMockWebhookStore(), nil)
	_, err := s.ChargeInvoice(context.Background(), "t1", invoices.invoices["inv_1"], testHold, "cus_stripe", "pm_test")
	if !errors.Is(err, ErrPaymentTransient) {
		t.Fatalf("err = %v, want ErrPaymentTransient", err)
	}
	if client.createCalls != 0 {
		t.Fatalf("Stripe received %d creates after the re-check refused", client.createCalls)
	}
	if invoices.confirms != 1 {
		t.Errorf("re-checks = %d, want 1", invoices.confirms)
	}
	if len(invoices.leaseReleases) != 1 {
		t.Errorf("releases = %d, want 1: nothing was sent, so the hold is released", len(invoices.leaseReleases))
	}

	// No hold at all (a caller that never claimed) is refused the same way.
	invoices.confirmRefused = false
	if _, err := s.ChargeInvoice(context.Background(), "t1", invoices.invoices["inv_1"], domain.ChargeHold{}, "cus_stripe", "pm_test"); !errors.Is(err, ErrPaymentTransient) {
		t.Fatalf("no hold: err = %v, want ErrPaymentTransient", err)
	}
	if client.createCalls != 0 {
		t.Fatalf("Stripe received %d creates from a caller holding no claim", client.createCalls)
	}
}

// With no Stripe mode configured the client is nil, and the reconciler still
// holds it: the stale-hold sweep's read answers "not configured", never a
// panic.
func TestListPaymentIntentsForCustomer_NoStripeConfigured(t *testing.T) {
	var c *LiveStripeClient
	if _, err := c.ListPaymentIntentsForCustomer(context.Background(), "cus_x", time.Now()); !errors.Is(err, ErrStripeNotConfigured) {
		t.Fatalf("err = %v, want ErrStripeNotConfigured", err)
	}
}

// A read Stripe never answered (a network error, a time limit) is
// ErrStripeUnreachable, which the stale-hold sweep counts toward stopping for
// the tick: each such read can take the whole time limit. A 500 is an answer
// and is not.
func TestListPaymentIntentsForCustomer_NoAnswerIsUnreachable(t *testing.T) {
	net := &noAnswer{}
	old := stripeHTTPClient.Transport
	stripeHTTPClient.Transport = net
	t.Cleanup(func() { stripeHTTPClient.Transport = old })
	c, ctx := listClient()
	_, err := c.ListPaymentIntentsForCustomer(ctx, "cus_x", time.Unix(1760000000, 0))
	if !errors.Is(err, ErrStripeUnreachable) || net.calls == 0 {
		t.Fatalf("no answer (%d requests): err = %v, want ErrStripeUnreachable", net.calls, err)
	}

	withRoutedStripe(t, map[string]func(*http.Request) (int, string){
		"GET /v1/payment_intents": func(*http.Request) (int, string) { return http.StatusInternalServerError, listPage500 },
	})
	_, err = c.ListPaymentIntentsForCustomer(ctx, "cus_x", time.Unix(1760000000, 0))
	if err == nil || errors.Is(err, ErrStripeUnreachable) {
		t.Fatalf("a 500: err = %v, want an error that is not ErrStripeUnreachable (Stripe answered)", err)
	}
}
