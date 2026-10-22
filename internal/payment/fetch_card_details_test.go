package payment

import (
	"net/http"
	"testing"
)

// FetchCardDetails reads the newest card on the Stripe customer and changes
// nothing at Stripe. It used to set that card as Stripe's default (ignoring
// errors), which split Stripe's copy from Velox's default, the card Velox
// charges: after a setup link, Stripe showed the new card while Velox kept
// charging the old one. Stripe's default now follows Velox's (ADR-053), so
// the read must send no customer update.
func TestFetchCardDetails_ReadsOnlyAndSendsNoCustomerUpdate(t *testing.T) {
	stub := withRoutedStripe(t, map[string]func(*http.Request) (int, string){
		"GET /v1/payment_methods": func(*http.Request) (int, string) {
			return http.StatusOK, `{"object":"list","url":"/v1/payment_methods","has_more":false,"data":[
				{"id":"pm_old","object":"payment_method","type":"card","created":1760000000,
				 "card":{"brand":"visa","last4":"0341","exp_month":1,"exp_year":2030}},
				{"id":"pm_new","object":"payment_method","type":"card","created":1760000500,
				 "card":{"brand":"visa","last4":"4242","exp_month":12,"exp_year":2034}}]}`
		},
		"POST /v1/customers/cus_x": func(*http.Request) (int, string) {
			return http.StatusOK, `{"id":"cus_x","object":"customer"}`
		},
	})
	c, ctx := listClient()

	got, err := c.FetchCardDetails(ctx, "cus_x")
	if err != nil {
		t.Fatalf("FetchCardDetails: %v", err)
	}
	if got.PaymentMethodID != "pm_new" || got.Last4 != "4242" {
		t.Fatalf("card = %+v, want the newest, pm_new ····4242", got)
	}
	for _, req := range stub.requests {
		if req.Method != http.MethodGet {
			t.Errorf("FetchCardDetails sent %s %s; it must only read", req.Method, req.URL.Path)
		}
	}
}
