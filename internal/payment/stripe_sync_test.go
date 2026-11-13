package payment

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tax"
)

// stripeRequest is one request the sync sent, with its form body decoded.
type stripeRequest struct {
	method string
	path   string
	form   url.Values
}

// recordStripe answers as Stripe would for a customer that holds one tax ID,
// txi_old (eu_vat DE111), and records every request. Nothing leaves the
// process.
func recordStripe(t *testing.T) (*StripeBillingSync, *[]stripeRequest) {
	t.Helper()
	var reqs []stripeRequest
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		form, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatalf("parse request body %q: %v", body, err)
		}
		reqs = append(reqs, stripeRequest{method: r.Method, path: r.URL.Path, form: form})

		reply := `{"id":"cus_1","object":"customer"}`
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/tax_ids"):
			reply = `{"object":"list","url":"/v1/customers/cus_1/tax_ids","has_more":false,"data":[{"id":"txi_old","object":"tax_id","type":"eu_vat","value":"DE111"}]}`
		case r.Method == http.MethodDelete:
			reply = `{"id":"txi_old","object":"tax_id","deleted":true}`
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/tax_ids"):
			reply = `{"id":"txi_new","object":"tax_id","type":"eu_vat","value":"DE222"}`
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(reply)),
		}, nil
	})
	clients := NewStripeClientsWithHTTPClient(fixedCreds{}, &http.Client{Transport: rt})
	return NewStripeBillingSync(clients), &reqs
}

func syncCtx() context.Context {
	return auth.WithTenantID(postgres.WithLivemode(context.Background(), true), "t1")
}

// The full sync makes the Stripe customer match the profile: its fields,
// its tax-exempt status, and exactly the profile's tax ID.
func TestSyncBillingProfile_PushesTheWholeProfile(t *testing.T) {
	sync, reqs := recordStripe(t)
	err := sync.SyncBillingProfile(syncCtx(), "cus_1", "billing@acme.test", domain.CustomerBillingProfile{
		LegalName: "Acme GmbH",
		Country:   "DE",
		TaxStatus: tax.StatusExempt,
		TaxIDType: "eu_vat",
		TaxID:     "DE222",
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	want := []stripeRequest{
		{method: http.MethodPost, path: "/v1/customers/cus_1", form: url.Values{
			"name":                 {"Acme GmbH"},
			"email":                {"billing@acme.test"},
			"tax_exempt":           {"exempt"},
			"address[line1]":       {""},
			"address[line2]":       {""},
			"address[city]":        {""},
			"address[state]":       {""},
			"address[postal_code]": {""},
			"address[country]":     {"DE"},
		}},
		{method: http.MethodGet, path: "/v1/customers/cus_1/tax_ids", form: url.Values{}},
		{method: http.MethodDelete, path: "/v1/customers/cus_1/tax_ids/txi_old", form: url.Values{}},
		{method: http.MethodPost, path: "/v1/customers/cus_1/tax_ids", form: url.Values{
			"type":  {"eu_vat"},
			"value": {"DE222"},
		}},
	}
	if !reflect.DeepEqual(*reqs, want) {
		t.Errorf("requests =\n%+v\nwant\n%+v", *reqs, want)
	}
}
