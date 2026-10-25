package pricing

import (
	"context"
	"net/http"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// TestUpdatePlan_OmittedDescriptionAndTaxCodeAreKept: PATCH /v1/plans/{id}
// leaves description and tax_code alone when the body omits them. The
// dashboard's edit dialog never sends either field, so a write-on-omit
// cleared a tax code set through the API and the base fee fell back to the
// tenant default tax code. Sending
// the key still writes it, and an empty string still clears it.
func TestUpdatePlan_OmittedDescriptionAndTaxCodeAreKept(t *testing.T) {
	const (
		desc    = "For growing teams"
		taxCode = "txcd_10103001"
	)

	seed := func(t *testing.T) (*Service, domain.Plan) {
		t.Helper()
		svc := NewService(newMemStore())
		p, _, err := svc.CreatePlan(context.Background(), "tnt_test", CreatePlanInput{
			Code: "pro", Name: "Pro", Currency: "USD", BillingInterval: domain.BillingMonthly,
			BaseAmountCents: 2900, BaseBillTiming: domain.BillInArrears, Description: desc, TaxCode: taxCode,
		})
		if err != nil {
			t.Fatalf("seed plan: %v", err)
		}
		return svc, p
	}

	cases := []struct {
		name     string
		body     string
		wantDesc string
		wantTax  string
	}{
		{"rename only", `{"name":"Pro 2"}`, desc, taxCode},
		{"archive only", `{"status":"archived"}`, desc, taxCode},
		{"base fee only", `{"base_amount_cents":3900}`, desc, taxCode},
		{"empty body", `{}`, desc, taxCode},
		{"explicit null keeps", `{"description":null,"tax_code":null}`, desc, taxCode},
		{"set both", `{"description":"New text","tax_code":"txcd_10000000"}`, "New text", "txcd_10000000"},
		{"empty strings clear", `{"description":"","tax_code":""}`, "", ""},
		{"whitespace is trimmed", `{"description":"  Spaced  ","tax_code":"  txcd_10000000 "}`, "Spaced", "txcd_10000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, p := seed(t)
			rr := doJSON(t, NewHandler(svc).updatePlan, http.MethodPatch, "/", tc.body, map[string]string{"id": p.ID})
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
	p, _, err := svc.CreatePlan(context.Background(), "tnt_test", CreatePlanInput{
		Code: "pro", Name: "Pro", Currency: "USD", BillingInterval: domain.BillingMonthly,
		TaxCode: "txcd_10103001",
	})
	if err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	rr := doJSON(t, NewHandler(svc).updatePlan, http.MethodPatch, "/", `{"name":"Pro 2","tax_code":"not-a-code"}`, map[string]string{"id": p.ID})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", rr.Code, rr.Body.String())
	}
	got, _ := svc.GetPlan(context.Background(), "tnt_test", p.ID)
	if got.Name != "Pro" || got.TaxCode != "txcd_10103001" {
		t.Errorf("refused PATCH wrote: name=%q tax_code=%q", got.Name, got.TaxCode)
	}
}
