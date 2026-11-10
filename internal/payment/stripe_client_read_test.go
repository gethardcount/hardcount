package payment

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// The PaymentIntent read by id (the reconciler's sweeps), decoded by
// stripe-go itself through stripeHTTPClient, so the code sees exactly what a
// real reply produces.

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
	readBadKey       = `{"error":{"message":"Invalid API Key provided: sk_test_****","type":"invalid_request_error"}}`
	read500          = `{"error":{"message":"An unknown error occurred","type":"api_error"}}`
	readNoPermission = `{"error":{"message":"The provided key 'rk_test_****' does not have the required permissions for this endpoint on account 'acct_x'. Having the 'rak_payment_intent_read' permission would allow this request to continue.","type":"invalid_request_error"}}`

	piSucceeded = `{"id":"pi_a","object":"payment_intent","status":"succeeded","created":1760000100,"amount_received":5000,
		"metadata":{"hardcount_invoice_id":"inv_1","hardcount_purpose":"dunning_retry"},
		"latest_charge":{"id":"ch_a","object":"charge","refunded":false}}`
	piRefunded = `{"id":"pi_b","object":"payment_intent","status":"succeeded","created":1760000050,"amount_received":5000,
		"metadata":{"hardcount_invoice_id":"inv_1"},
		"latest_charge":{"id":"ch_b","object":"charge","refunded":true}}`
	piCanceledNoCharge = `{"id":"pi_c","object":"payment_intent","status":"canceled","created":1760000010,"amount_received":0,
		"metadata":{"hardcount_invoice_id":"inv_1"},"latest_charge":null}`
)

func readClient() (*LiveStripeClient, context.Context) {
	return NewLiveStripeClient(NewStripeClients(fixedCreds{})), auth.WithTenantID(postgres.WithLivemode(context.Background(), true), "t1")
}

// The read carries what the sweeps act on: the invoice, purpose, created
// time and captured amount, and whether a confirm ever reached a bank
// (Charged: latest_charge is set, as an object or as its id). A payment
// refunded at Stripe since is read as the payment it was.
func TestGetPaymentIntent_ReadsWhatTheSweepsActOn(t *testing.T) {
	stub := withRoutedStripe(t, map[string]func(*http.Request) (int, string){
		"GET /v1/payment_intents/pi_a": func(*http.Request) (int, string) { return http.StatusOK, piSucceeded },
		"GET /v1/payment_intents/pi_b": func(*http.Request) (int, string) { return http.StatusOK, piRefunded },
		"GET /v1/payment_intents/pi_c": func(*http.Request) (int, string) { return http.StatusOK, piCanceledNoCharge },
		"GET /v1/payment_intents/pi_u": func(*http.Request) (int, string) {
			return http.StatusOK, `{"id":"pi_u","object":"payment_intent","status":"succeeded","amount_received":5000,"latest_charge":"ch_u"}`
		},
	})
	c, ctx := readClient()
	a, err := c.GetPaymentIntent(ctx, "pi_a")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != domain.ProviderPISucceeded || a.InvoiceID != "inv_1" || a.Purpose != "dunning_retry" || a.AmountReceivedCents != 5000 || a.CreatedAt != 1760000100 || !a.Charged {
		t.Errorf("pi_a = %+v", a)
	}
	if q := stub.requests[0].URL.Query(); len(q) != 0 {
		t.Errorf("read query = %v, want a plain read: a restricted key may read PaymentIntents and not charges", q)
	}
	if b, err := c.GetPaymentIntent(ctx, "pi_b"); err != nil || b.Status != domain.ProviderPISucceeded || !b.Charged || b.AmountReceivedCents != 5000 {
		t.Errorf("pi_b = %+v, %v; want succeeded and Charged: a refund made at Stripe does not unmake the payment", b, err)
	}
	if u, err := c.GetPaymentIntent(ctx, "pi_u"); err != nil || !u.Charged {
		t.Errorf("pi_u = %+v, %v; want Charged from latest_charge given as its id", u, err)
	}
	if cn, err := c.GetPaymentIntent(ctx, "pi_c"); err != nil || cn.Charged || cn.Status != "canceled" {
		t.Errorf("pi_c = %+v, %v; want canceled and not Charged (no confirm reached a bank)", cn, err)
	}
}

// A failed read is an error that carries a *PaymentError, whatever refused
// it: a rejected key, a restricted key without read access, a 500, a
// PaymentIntent the account does not have. The sweep reports it and reads
// again on a later tick; the invoice stays as it is.
func TestGetPaymentIntent_AFailedReadIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"API key rejected", http.StatusUnauthorized, readBadKey},
		{"API key lacks permission (403)", http.StatusForbidden, readNoPermission},
		{"a 500", http.StatusInternalServerError, read500},
		{"not in this account", http.StatusNotFound, `{"error":{"code":"resource_missing","message":"No such payment_intent: 'pi_x'","param":"intent","type":"invalid_request_error"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withRoutedStripe(t, map[string]func(*http.Request) (int, string){
				"GET /v1/payment_intents/pi_x": func(*http.Request) (int, string) { return tc.status, tc.body },
			})
			c, ctx := readClient()
			got, err := c.GetPaymentIntent(ctx, "pi_x")
			var pe *PaymentError
			if err == nil || !errors.As(err, &pe) || got.Status != "" {
				t.Fatalf("got %+v, err = %#v; want no result and an error carrying a *PaymentError", got, err)
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
	_, err := s.ChargeInvoice(postgres.WithLivemode(context.Background(), true), "t1", invoices.invoices["inv_1"], testHold, "cus_stripe", "pm_test")
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
	if _, err := s.ChargeInvoice(postgres.WithLivemode(context.Background(), true), "t1", invoices.invoices["inv_1"], domain.ChargeHold{}, "cus_stripe", "pm_test"); !errors.Is(err, ErrPaymentTransient) {
		t.Fatalf("no hold: err = %v, want ErrPaymentTransient", err)
	}
	if client.createCalls != 0 {
		t.Fatalf("Stripe received %d creates from a caller holding no claim", client.createCalls)
	}
}

// With no Stripe mode configured the client is nil, and the reconciler still
// holds it: its read answers "not configured", never a panic.
func TestGetPaymentIntent_NoStripeConfigured(t *testing.T) {
	var c *LiveStripeClient
	if _, err := c.GetPaymentIntent(context.Background(), "pi_x"); !errors.Is(err, ErrStripeNotConfigured) {
		t.Fatalf("err = %v, want ErrStripeNotConfigured", err)
	}
}

// A read Stripe never answered (a network error, a time limit) is an unclear
// error, never a result.
func TestGetPaymentIntent_NoAnswerIsAnError(t *testing.T) {
	net := &noAnswer{}
	old := stripeHTTPClient.Transport
	stripeHTTPClient.Transport = net
	t.Cleanup(func() { stripeHTTPClient.Transport = old })
	c, ctx := readClient()
	var pe *PaymentError
	if _, err := c.GetPaymentIntent(ctx, "pi_x"); !errors.As(err, &pe) || !pe.Unknown || net.calls == 0 {
		t.Fatalf("no answer (%d requests): err = %v, want an unclear *PaymentError", net.calls, err)
	}
}

// A confirm Stripe refuses as payment_intent_unexpected_state is not a
// decline: an earlier confirm already moved the PaymentIntent on (a resend
// after a lost reply). ConfirmPaymentIntent reads it and returns its real
// state; read by its type (invalid_request_error) it would mark a paid
// invoice failed.
func TestConfirmPaymentIntent_UnexpectedState_ReturnsRealState(t *testing.T) {
	const unexpected = `{"error":{"code":"payment_intent_unexpected_state","message":"You cannot confirm this PaymentIntent because it has a status of succeeded.","type":"invalid_request_error"}}`
	for _, tc := range []struct {
		name, got  string
		wantStatus string // "" = want an unclear PaymentError naming the PaymentIntent
	}{
		{"already succeeded", `{"id":"pi_x","object":"payment_intent","status":"succeeded","amount_received":5000,"latest_charge":{"id":"ch_x","object":"charge"}}`, "succeeded"},
		{"already processing", `{"id":"pi_x","object":"payment_intent","status":"processing"}`, "processing"},
		{"canceled", `{"id":"pi_x","object":"payment_intent","status":"canceled"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withRoutedStripe(t, map[string]func(*http.Request) (int, string){
				"POST /v1/payment_intents/pi_x/confirm": func(*http.Request) (int, string) { return http.StatusBadRequest, unexpected },
				"GET /v1/payment_intents/pi_x":          func(*http.Request) (int, string) { return http.StatusOK, tc.got },
			})
			c, ctx := readClient()
			res, err := c.ConfirmPaymentIntent(ctx, "pi_x", PaymentIntentParams{AmountCents: 5000, Currency: "usd", CustomerID: "cus_x", PaymentMethodID: "pm_x", IdempotencyKey: "k"})
			if tc.wantStatus != "" {
				if err != nil || res.Status != tc.wantStatus || res.ID != "pi_x" {
					t.Fatalf("res %+v err %v, want %s on pi_x (never a decline)", res, err, tc.wantStatus)
				}
				return
			}
			var pe *PaymentError
			if !errors.As(err, &pe) || !pe.Unknown || pe.PaymentIntentID != "pi_x" {
				t.Fatalf("err = %#v, want an unclear PaymentError naming pi_x (not a decline)", err)
			}
		})
	}
}
