package pricing

import (
	"context"
	"net/http"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// TestUpdatePlan_OmittedDescriptionAndTaxCodeAreKept: PATCH /v1/plans/{id}
// leaves description and tax_code alone when the body omits them. The
// dashboard's meter attach sends only meter_ids, and its edit dialog never
// sends either field, so a write-on-omit cleared a tax code set through the
// API and the base fee fell back to the tenant default tax code. Sending
// the key still writes it, and an empty string still clears it.
func TestUpdatePlan_OmittedDescriptionAndTaxCodeAreKept(t *testing.T) {
	const (
		desc    = "For growing teams"
		taxCode = "txcd_10103001"
	)

	seed := func(t *testing.T) (*Service, domain.Plan, string) {
		t.Helper()
		svc := NewService(newMemStore())
		meterID, _ := seedMeterAndRule(t, svc, "tnt_test")
		p, err := svc.CreatePlan(context.Background(), "tnt_test", CreatePlanInput{
			Code: "pro", Name: "Pro", Currency: "USD", BillingInterval: domain.BillingMonthly,
			BaseAmountCents: 2900, Description: desc, TaxCode: taxCode,
		})
		if err != nil {
			t.Fatalf("seed plan: %v", err)
		}
		return svc, p, meterID
	}

	cases := []struct {
		name     string
		body     func(meterID string) string
		wantDesc string
		wantTax  string
	}{
		{"attach a meter (dashboard)", func(m string) string { return `{"meter_ids":["` + m + `"]}` }, desc, taxCode},
		{"rename only", func(string) string { return `{"name":"Pro 2"}` }, desc, taxCode},
		{"archive only", func(string) string { return `{"status":"archived"}` }, desc, taxCode},
		{"empty body", func(string) string { return `{}` }, desc, taxCode},
		{"explicit null keeps", func(string) string { return `{"description":null,"tax_code":null}` }, desc, taxCode},
		{"set both", func(string) string { return `{"description":"New text","tax_code":"txcd_10000000"}` }, "New text", "txcd_10000000"},
		{"empty strings clear", func(string) string { return `{"description":"","tax_code":""}` }, "", ""},
		{"whitespace is trimmed", func(string) string { return `{"description":"  Spaced  ","tax_code":"  txcd_10000000 "}` }, "Spaced", "txcd_10000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, p, meterID := seed(t)
			rr := doJSON(t, NewHandler(svc).updatePlan, http.MethodPatch, "/", tc.body(meterID), map[string]string{"id": p.ID})
			if rr.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
			}
			got, err := svc.GetPlan(context.Background(), "tnt_test", p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Description != tc.wantDesc {
				t.Errorf("description = %q, want %q", got.Description, tc.wantDesc)
			}
			if got.TaxCode != tc.wantTax {
				t.Errorf("tax_code = %q, want %q", got.TaxCode, tc.wantTax)
			}
		})
	}
}

// TestUpdatePlan_InvalidTaxCodeRefused: a sent tax_code is still checked,
// and a refused PATCH writes nothing.
func TestUpdatePlan_InvalidTaxCodeRefused(t *testing.T) {
	svc := NewService(newMemStore())
	p, err := svc.CreatePlan(context.Background(), "tnt_test", CreatePlanInput{
		Code: "pro", Name: "Pro", Currency: "USD", BillingInterval: domain.BillingMonthly,
		TaxCode: "txcd_10103001",
	})
	if err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	rr := doJSON(t, NewHandler(svc).updatePlan, http.MethodPatch, "/", `{"name":"Pro 2","tax_code":"not-a-code"}`, map[string]string{"id": p.ID})
	if rr.Code != http.StatusUnprocessableEntity && rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want a 4xx refusal: %s", rr.Code, rr.Body.String())
	}
	got, _ := svc.GetPlan(context.Background(), "tnt_test", p.ID)
	if got.Name != "Pro" || got.TaxCode != "txcd_10103001" {
		t.Errorf("refused PATCH wrote: name=%q tax_code=%q", got.Name, got.TaxCode)
	}
}
