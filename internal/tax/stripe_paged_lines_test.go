package tax

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stripe/stripe-go/v82"
)

// An invoice with more lines than the expanded line_items page carries comes
// back has_more=true; the remaining lines are paged from the calculation's
// line_items endpoint, so every line gets its own tax (and the "every line
// mapped" check does not fail a legitimate long invoice).
func TestCalculate_PagesTheRemainingLineItems(t *testing.T) {
	const n = 13
	item := func(i int) string {
		return fmt.Sprintf(`{"id":"tax_li_%d","object":"tax.calculation_line_item","amount":1000,"amount_tax":%d,"reference":"line_%d"}`, i, 100+i, i)
	}
	items := func(from, to int) string {
		var parts []string
		for i := from; i < to; i++ {
			parts = append(parts, item(i))
		}
		return strings.Join(parts, ",")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tax/calculations":
			_, _ = fmt.Fprintf(w, `{"id":"taxcalc_1","object":"tax.calculation","tax_amount_exclusive":%d,
				"line_items":{"object":"list","data":[%s],"has_more":true,"url":"/v1/tax/calculations/taxcalc_1/line_items"}}`, 1378, items(0, 10))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tax/calculations/taxcalc_1/line_items":
			if r.URL.Query().Get("starting_after") == "" {
				_, _ = fmt.Fprintf(w, `{"object":"list","data":[%s],"has_more":true,"url":"/v1/tax/calculations/taxcalc_1/line_items"}`, items(0, 10))
				return
			}
			_, _ = fmt.Fprintf(w, `{"object":"list","data":[%s],"has_more":false,"url":"/v1/tax/calculations/taxcalc_1/line_items"}`, items(10, n))
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	be := stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{URL: stripe.String(srv.URL), MaxNetworkRetries: stripe.Int64(0)})
	client := stripe.NewClient("sk_test_fake", stripe.WithBackends(&stripe.Backends{API: be, Connect: be, Uploads: be, MeterEvents: be}))
	p := NewStripeTaxProvider(staticClientResolver{c: client})

	req := Request{Currency: "usd", CustomerAddress: Address{Country: "US", State: "WA", PostalCode: "98101"}}
	for i := 0; i < n; i++ {
		req.LineItems = append(req.LineItems, RequestLine{Ref: fmt.Sprintf("line_%d", i), AmountCents: 1000, Quantity: 1})
	}
	res, err := p.Calculate(context.Background(), req)
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	for i, l := range res.Lines {
		if l.TaxAmountCents != int64(100+i) {
			t.Errorf("line %d tax = %d, want %d", i, l.TaxAmountCents, 100+i)
		}
	}
}
