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
	"github.com/sagarsuperuser/velox/internal/tenantstripe"
)

// The interrupted-charge check's read (ADR-127) and the charge path's error
// classes, decoded by stripe-go itself through stripeHTTPClient, so the code
// sees exactly what a real reply produces.

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
	customerLive    = `{"id":"cus_x","object":"customer","deleted":false}`
	customerDeleted = `{"id":"cus_x","object":"customer","deleted":true}`
	customerMissing = `{"error":{"code":"resource_missing","message":"No such customer: 'cus_x'","param":"id","type":"invalid_request_error"}}`
	listPage500     = `{"error":{"message":"An unknown error occurred","type":"api_error"}}`

	piPage1 = `{"object":"list","url":"/v1/payment_intents","has_more":true,"data":[
		{"id":"pi_a","object":"payment_intent","status":"succeeded","created":1760000100,"amount_received":5000,
		 "metadata":{"velox_attempt_key":"velox_inv_inv_1_3_pm_x","velox_invoice_id":"inv_1"}},
		{"id":"pi_b","object":"payment_intent","status":"requires_payment_method","created":1760000050,"amount_received":0,
		 "metadata":{"velox_attempt_key":"velox_inv_inv_1_2_pm_x","velox_purpose":"dunning_retry","velox_invoice_id":"inv_1"},
		 "last_payment_error":{"type":"card_error","code":"card_declined","decline_code":"lost_card","message":"Your card was declined.",
		   "payment_method":{"id":"pm_x","object":"payment_method"}}}]}`
	piPage2 = `{"object":"list","url":"/v1/payment_intents","has_more":false,"data":[
		{"id":"pi_c","object":"payment_intent","status":"processing","created":1760000010,"amount_received":0,
		 "metadata":{"velox_invoice_id":"inv_1"}}]}`
)

func listClient() (*LiveStripeClient, context.Context) {
	return NewLiveStripeClient(NewStripeClients(fixedCreds{})), auth.WithTenantID(context.Background(), "t1")
}

// Every page is read, each PaymentIntent carries what the check needs, and
// the request asks for this customer from the given time on.
func TestListPaymentIntentsForCustomer_ReadsEveryPage(t *testing.T) {
	stub := withRoutedStripe(t, map[string]func(*http.Request) (int, string){
		"GET /v1/customers/cus_x": func(*http.Request) (int, string) { return http.StatusOK, customerLive },
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
	if a.ID != "pi_a" || a.Status != "succeeded" || a.AttemptKey != "velox_inv_inv_1_3_pm_x" || a.AmountReceivedCents != 5000 || a.CreatedAt != 1760000100 {
		t.Errorf("pi_a = %+v", a)
	}
	if b.AttemptKey != "velox_inv_inv_1_2_pm_x" || b.Purpose != "dunning_retry" || b.DeclineCode != "lost_card" ||
		b.PaymentMethodID != "pm_x" || b.FailureMessage != "Your card was declined." {
		t.Errorf("pi_b = %+v, want its key, purpose, decline code, card and message", b)
	}
	if c3.ID != "pi_c" || c3.AttemptKey != "" {
		t.Errorf("pi_c = %+v, want no attempt key (a PaymentIntent from before velox_attempt_key)", c3)
	}

	var lists []*http.Request
	for _, req := range stub.requests {
		if req.URL.Path == "/v1/payment_intents" {
			lists = append(lists, req)
		}
	}
	if len(lists) != 2 {
		t.Fatalf("list requests = %d, want 2", len(lists))
	}
	q := lists[0].URL.Query()
	if q.Get("customer") != "cus_x" || q.Get("created[gte]") != "1760000000" {
		t.Errorf("list query = %v, want customer=cus_x and created[gte]=1760000000", q)
	}
}

// A failed read is never "no such charge": a customer missing (deleted, or
// in another Stripe account) or an error on any page fails the whole read.
func TestListPaymentIntentsForCustomer_AFailedReadIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		routes map[string]func(*http.Request) (int, string)
	}{
		{"customer deleted", map[string]func(*http.Request) (int, string){
			"GET /v1/customers/cus_x": func(*http.Request) (int, string) { return http.StatusOK, customerDeleted },
			"GET /v1/payment_intents": func(*http.Request) (int, string) { return http.StatusOK, piPage2 },
		}},
		{"customer missing (deleted, or in another account)", map[string]func(*http.Request) (int, string){
			"GET /v1/customers/cus_x": func(*http.Request) (int, string) { return http.StatusNotFound, customerMissing },
			"GET /v1/payment_intents": func(*http.Request) (int, string) { return http.StatusOK, piPage2 },
		}},
		{"error on the second page", map[string]func(*http.Request) (int, string){
			"GET /v1/customers/cus_x": func(*http.Request) (int, string) { return http.StatusOK, customerLive },
			"GET /v1/payment_intents": func(req *http.Request) (int, string) {
				if req.URL.Query().Get("starting_after") == "" {
					return http.StatusOK, piPage1
				}
				return http.StatusInternalServerError, listPage500
			},
		}},
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
		})
	}
}

// The "same key, different parameters" 409 is told apart from the other 409s
// (the charge path parks only on it), and a 400 stays a definite rejection.
func TestClassifyStripeError_IdempotencyConflictIsMarked(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		conflict bool
		unknown  bool
	}{
		{"409 same key, different parameters", http.StatusConflict, stripe409ParamsDiffer, true, true},
		{"409 key in use", http.StatusConflict, stripe409KeyInUse, false, true},
		{"400 missing parameter", http.StatusBadRequest, stripe400MissingParam, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withCannedStripe(t, tc.status, tc.body)
			c, ctx := listClient()
			_, err := c.CreatePaymentIntent(ctx, PaymentIntentParams{AmountCents: 100, Currency: "usd", CustomerID: "cus_x", PaymentMethodID: "pm_x", IdempotencyKey: "k"})
			var pe *PaymentError
			if !errors.As(err, &pe) {
				t.Fatalf("err = %#v, want a *PaymentError", err)
			}
			if pe.IdempotencyConflict != tc.conflict || pe.Unknown != tc.unknown {
				t.Errorf("IdempotencyConflict=%v Unknown=%v, want %v / %v", pe.IdempotencyConflict, pe.Unknown, tc.conflict, tc.unknown)
			}
		})
	}
}

type unreadableCreds struct{}

func (unreadableCreds) GetPlaintext(context.Context, string, bool) (tenantstripe.PlaintextSecrets, error) {
	return tenantstripe.PlaintextSecrets{}, errors.New("connection pool exhausted")
}

// Credentials that cannot be read mean no request left Velox (ErrNotSent): the
// charge clears its record. A 429 does not: an earlier request stripe-go sent
// under the same key may still be running, so the record stays.
func TestCreatePaymentIntent_NotSentIsToldApartFromRateLimit(t *testing.T) {
	ctx := auth.WithTenantID(context.Background(), "t1")
	params := PaymentIntentParams{AmountCents: 100, Currency: "usd", CustomerID: "cus_x", PaymentMethodID: "pm_x", IdempotencyKey: "k"}

	_, err := NewLiveStripeClient(NewStripeClients(unreadableCreds{})).CreatePaymentIntent(ctx, params)
	if !errors.Is(err, ErrPaymentTransient) || !errors.Is(err, ErrNotSent) {
		t.Errorf("unreadable credentials: err = %v, want ErrPaymentTransient and ErrNotSent", err)
	}

	withCannedStripe(t, http.StatusTooManyRequests, stripe429)
	_, err = NewLiveStripeClient(NewStripeClients(fixedCreds{})).CreatePaymentIntent(ctx, params)
	if !errors.Is(err, ErrPaymentTransient) || errors.Is(err, ErrNotSent) {
		t.Errorf("429: err = %v, want ErrPaymentTransient without ErrNotSent", err)
	}
}

// Stripe receives exactly the key ChargeIdempotencyKey built: nothing is
// appended on the way out.
func TestCreatePaymentIntent_SendsTheKeyUnchanged(t *testing.T) {
	stub := withScriptedStripe(t, scriptedStep{status: http.StatusOK,
		body: `{"id":"pi_1","object":"payment_intent","status":"succeeded"}`})
	key := ChargeIdempotencyKey("inv_1", 4, "", "pm_x")
	c, ctx := listClient()
	if _, err := c.CreatePaymentIntent(ctx, PaymentIntentParams{AmountCents: 100, Currency: "usd", CustomerID: "cus_x", PaymentMethodID: "pm_x", IdempotencyKey: key}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(stub.keys) != 1 || stub.keys[0] != key {
		t.Errorf("Idempotency-Key sent = %q, want %q", stub.keys, key)
	}
}
