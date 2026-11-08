package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/stripe/stripe-go/v82"

	"github.com/gethardcount/hardcount/internal/platform/money"
)

// stripeTaxFake stands in for the Stripe Tax API over HTTP. A test wires the
// real tax.StripeTaxProvider to it (it is the provider's client resolver), so
// the provider's own request building and response reading run. It taxes
// every line at one rate, as Stripe documents the two tax behaviors:
//
//   - exclusive: the line amount is before tax; amount_tax = amount × rate.
//   - inclusive: the line amount includes the tax, and Stripe returns it
//     unchanged with the tax inside it: amount_tax = amount − amount ÷ (1 + rate).
//
// Both round half to even. Each calculation request is recorded.
type stripeTaxFake struct {
	client *stripe.Client

	mu       sync.Mutex
	rateBP   int64
	failNext bool
	requests []stripeTaxFakeRequest
}

// stripeTaxFakeRequest is what one calculation request asked for.
type stripeTaxFakeRequest struct {
	Amounts   []int64
	Inclusive bool
}

// newStripeTaxFake starts the fake at rateBP basis points (2000 = 20%).
func newStripeTaxFake(t testing.TB, rateBP int64) *stripeTaxFake {
	t.Helper()
	f := &stripeTaxFake{rateBP: rateBP}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	be := stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{URL: stripe.String(srv.URL), MaxNetworkRetries: stripe.Int64(0)})
	f.client = stripe.NewClient("sk_test_fake", stripe.WithBackends(&stripe.Backends{API: be, Connect: be, Uploads: be, MeterEvents: be}))
	return f
}

// ResolveCtx makes the fake the provider's Stripe client resolver.
func (f *stripeTaxFake) ResolveCtx(context.Context) (*stripe.Client, error) { return f.client, nil }

// SetRate changes the rate for the calculations that follow, as a rate
// change or a customer's move to another country does.
func (f *stripeTaxFake) SetRate(bp int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rateBP = bp
}

// FailNext makes the next calculation fail with a 500, as an outage does.
// The failed request is still recorded.
func (f *stripeTaxFake) FailNext() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext = true
}

// Requests returns the calculation requests so far, oldest first.
func (f *stripeTaxFake) Requests() []stripeTaxFakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]stripeTaxFakeRequest(nil), f.requests...)
}

func (f *stripeTaxFake) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/tax/calculations" {
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	req := stripeTaxFakeRequest{}
	type line struct {
		Object      string `json:"object"`
		ID          string `json:"id"`
		Reference   string `json:"reference"`
		Amount      int64  `json:"amount"`
		AmountTax   int64  `json:"amount_tax"`
		Quantity    int64  `json:"quantity"`
		TaxBehavior string `json:"tax_behavior"`
	}
	var lines []line
	var taxInclusive, taxExclusive int64
	for i := 0; ; i++ {
		key := fmt.Sprintf("line_items[%d]", i)
		raw := r.PostForm.Get(key + "[amount]")
		if raw == "" {
			break
		}
		amount, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		behavior := r.PostForm.Get(key + "[tax_behavior]")
		var lineTax int64
		var ok bool
		if behavior == "inclusive" {
			req.Inclusive = true
			var net int64
			if net, ok = money.MulDivRoundHalfToEvenSigned(amount, 10000, 10000+f.rateBP); ok {
				lineTax = amount - net
				taxInclusive += lineTax
			}
		} else {
			if lineTax, ok = money.MulDivRoundHalfToEvenSigned(amount, f.rateBP, 10000); ok {
				taxExclusive += lineTax
			}
		}
		if !ok {
			http.Error(w, "amount out of range", http.StatusBadRequest)
			return
		}
		req.Amounts = append(req.Amounts, amount)
		lines = append(lines, line{
			Object: "tax.calculation_line_item", ID: fmt.Sprintf("tax_li_%d", i),
			Reference: r.PostForm.Get(key + "[reference]"), Amount: amount, AmountTax: lineTax,
			Quantity: 1, TaxBehavior: behavior,
		})
	}
	f.requests = append(f.requests, req)
	if f.failNext {
		f.failNext = false
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, `{"error":{"type":"api_error","message":"fake outage"}}`) // test server: a short write fails the client's decode
		return
	}
	id := fmt.Sprintf("taxcalc_fake_%d", len(f.requests))

	body, err := json.Marshal(map[string]any{
		"id":                   id,
		"object":               "tax.calculation",
		"currency":             r.PostForm.Get("currency"),
		"tax_amount_inclusive": taxInclusive,
		"tax_amount_exclusive": taxExclusive,
		"tax_breakdown": []map[string]any{{
			"amount":            taxInclusive + taxExclusive,
			"inclusive":         req.Inclusive,
			"taxability_reason": "standard_rated",
			"tax_rate_details": map[string]any{
				"country":            "GB",
				"percentage_decimal": strconv.FormatFloat(float64(f.rateBP)/100, 'f', 4, 64),
				"tax_type":           "vat",
			},
		}},
		"line_items": map[string]any{
			"object": "list", "data": lines, "has_more": false,
			"url": "/v1/tax/calculations/" + id + "/line_items",
		},
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body) // test server: a short write fails the client's decode
}
