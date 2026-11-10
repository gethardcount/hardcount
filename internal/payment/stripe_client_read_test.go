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
// time and captured amount; whether a confirm ever reached a bank (Charged);
// and whether the charge was refunded in full, which needs latest_charge
// expanded (unexpanded it is only an id).
func TestGetPaymentIntent_ReadsWhatTheSweepsActOn(t *testing.T) {
	stub := withRoutedStripe(t, map[string]func(*http.Request) (int, string){
		"GET /v1/payment_intents/pi_a": func(*http.Request) (int, string) { return http.StatusOK, piSucceeded },
		"GET /v1/payment_intents/pi_b": func(*http.Request) (int, string) { return http.StatusOK, piRefunded },
		"GET /v1/payment_intents/pi_c": func(*http.Request) (int, string) { return http.StatusOK, piCanceledNoCharge },
	})
	c, ctx := readClient()
	a, err := c.GetPaymentIntent(ctx, "pi_a")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != domain.ProviderPISucceeded || a.InvoiceID != "inv_1" || a.Purpose != "dunning_retry" || a.AmountReceivedCents != 5000 || a.CreatedAt != 1760000100 || !a.Charged || a.FullyRefunded {
		t.Errorf("pi_a = %+v", a)
	}
	if q := stub.requests[0].URL.Query(); len(q["expand[0]"]) != 1 || q["expand[0]"][0] != "latest_charge" {
		t.Errorf("read query = %v, want expand[0]=latest_charge (the refund check reads it)", q)
	}
	if b, err := c.GetPaymentIntent(ctx, "pi_b"); err != nil || !b.FullyRefunded || !b.Charged {
		t.Errorf("pi_b = %+v, %v; want FullyRefunded and Charged", b, err)
	}
	if cn, err := c.GetPaymentIntent(ctx, "pi_c"); err != nil || cn.Charged || cn.Status != "canceled" {
		t.Errorf("pi_c = %+v, %v; want canceled and not Charged (no confirm reached a bank)", cn, err)
	}
}

// A failed read is typed for the sweeps: a refused key (401, or a 403 for a
// restricted key without PaymentIntent read access) will not heal on retry;
// a 500 is an answer that may. Each still carries a *PaymentError.
func TestGetPaymentIntent_AFailedReadIsTyped(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error // nil: an error that is not a refusal
	}{
		{"API key rejected", http.StatusUnauthorized, readBadKey, ErrStripeAuthFailed},
		{"API key lacks permission (403)", http.StatusForbidden, readNoPermission, ErrStripeAuthFailed},
		{"a 500", http.StatusInternalServerError, read500, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withRoutedStripe(t, map[string]func(*http.Request) (int, string){
				"GET /v1/payment_intents/pi_x": func(*http.Request) (int, string) { return tc.status, tc.body },
			})
			c, ctx := readClient()
			_, err := c.GetPaymentIntent(ctx, "pi_x")
			var pe *PaymentError
			if err == nil || !errors.As(err, &pe) {
				t.Fatalf("err = %#v, want an error carrying a *PaymentError", err)
			}
			switch {
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Errorf("err = %v, want %v", err, tc.want)
			case tc.want == nil && (errors.Is(err, ErrStripeAuthFailed) || errors.Is(err, ErrStripeUnreachable)):
				t.Errorf("err = %v: an answer that may heal was typed as a refusal or as no answer", err)
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

// A read Stripe never answered (a network error, a time limit) is
// ErrStripeUnreachable, which the stale-hold sweep counts toward stopping for
// the tick: each such read can take the whole time limit.
func TestGetPaymentIntent_NoAnswerIsUnreachable(t *testing.T) {
	net := &noAnswer{}
	old := stripeHTTPClient.Transport
	stripeHTTPClient.Transport = net
	t.Cleanup(func() { stripeHTTPClient.Transport = old })
	c, ctx := readClient()
	if _, err := c.GetPaymentIntent(ctx, "pi_x"); !errors.Is(err, ErrStripeUnreachable) || net.calls == 0 {
		t.Fatalf("no answer (%d requests): err = %v, want ErrStripeUnreachable", net.calls, err)
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

// A PaymentIntent the account does not have (resource_missing) is a refusal
// that will not heal: the sweeps report it instead of retrying quietly.
func TestGetPaymentIntent_MissingFromTheAccount(t *testing.T) {
	withRoutedStripe(t, map[string]func(*http.Request) (int, string){
		"GET /v1/payment_intents/pi_gone": func(*http.Request) (int, string) {
			return http.StatusNotFound, `{"error":{"code":"resource_missing","message":"No such payment_intent: 'pi_gone'","param":"intent","type":"invalid_request_error"}}`
		},
	})
	c, ctx := readClient()
	_, err := c.GetPaymentIntent(ctx, "pi_gone")
	if !errors.Is(err, ErrStripePaymentIntentMissing) {
		t.Fatalf("err = %v, want ErrStripePaymentIntentMissing", err)
	}
}

// A restricted key that may read PaymentIntents but not charges is refused
// the expanded read; the read falls back to the unexpanded PaymentIntent.
func TestGetPaymentIntent_RestrictedKeyWithoutCharges_ReadsUnexpanded(t *testing.T) {
	withRoutedStripe(t, map[string]func(*http.Request) (int, string){
		"GET /v1/payment_intents/pi_r": func(req *http.Request) (int, string) {
			if req.URL.Query().Get("expand[0]") != "" {
				return http.StatusForbidden, readNoPermission
			}
			return http.StatusOK, `{"id":"pi_r","object":"payment_intent","status":"succeeded","amount_received":5000,"latest_charge":"ch_r"}`
		},
	})
	c, ctx := readClient()
	got, err := c.GetPaymentIntent(ctx, "pi_r")
	if err != nil || got.Status != domain.ProviderPISucceeded || !got.Charged {
		t.Fatalf("got %+v, %v; want succeeded and Charged from the unexpanded read", got, err)
	}
}
