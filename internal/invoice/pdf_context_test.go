package invoice

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
)

// P6: PDF context consolidation + comms correctness (audit High #11 +
// invoice-comms mediums).

// TestRenderPDF_ConcurrentCurrencies: the currency symbol was a
// package-level var mutated at the top of every render — CI runs this
// suite with -race, so concurrent renders with different currencies
// re-introduce a detector hit if anyone resurrects the global.
func TestRenderPDF_ConcurrentCurrencies(t *testing.T) {
	now := time.Now().UTC()
	mkInvoice := func(cur string) domain.Invoice {
		return domain.Invoice{
			InvoiceNumber: "INV-" + cur, Currency: cur,
			Status: domain.InvoiceFinalized, SubtotalCents: 12345,
			TotalAmountCents: 12345, AmountDueCents: 12345,
			BillingPeriodStart: now.Add(-30 * 24 * time.Hour), BillingPeriodEnd: now,
			IssuedAt: &now,
		}
	}
	items := []domain.InvoiceLineItem{{
		LineType: domain.LineTypeBaseFee, Description: "Base",
		Quantity: 1, UnitAmountCents: 12345, AmountCents: 12345, TotalAmountCents: 12345,
	}}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		cur := "USD"
		if i%2 == 1 {
			cur = "EUR"
		}
		wg.Add(1)
		go func(cur string) {
			defer wg.Done()
			if _, err := RenderPDF(context.Background(), mkInvoice(cur), items, BillToInfo{Name: "C"}, nil); err != nil {
				t.Errorf("render %s: %v", cur, err)
			}
		}(cur)
	}
	wg.Wait()
}

// TestFormatCentsIn_SymbolIsThreaded: the formatters take the symbol as
// a parameter — per-render, never shared state.
func TestFormatCentsIn_SymbolIsThreaded(t *testing.T) {
	if got := formatCentsIn("€", 123456); got != "€1,234.56" {
		t.Errorf("EUR: got %q", got)
	}
	if got := formatCentsIn("$", -50); got != "-$0.50" {
		t.Errorf("negative: got %q", got)
	}
	if got := formatCentsIn("SEK ", 0); got != "SEK 0.00" {
		t.Errorf("fallback zero: got %q", got)
	}
	if got := currencySymbolFor("sek"); got != "sek " {
		t.Errorf("unmapped currency fallback: got %q", got)
	}
}

type fakePDFCustomers struct {
	cust domain.Customer
	bp   domain.CustomerBillingProfile
}

func (f *fakePDFCustomers) Get(_ context.Context, _, _ string) (domain.Customer, error) {
	return f.cust, nil
}
func (f *fakePDFCustomers) GetBillingProfile(_ context.Context, _, _ string) (domain.CustomerBillingProfile, error) {
	return f.bp, nil
}

type fakePDFSettings struct{ ts domain.TenantSettings }

func (f *fakePDFSettings) Get(_ context.Context, _ string) (domain.TenantSettings, error) {
	return f.ts, nil
}

type fakePDFCreditNotes struct{ notes []domain.CreditNote }

func (f *fakePDFCreditNotes) List(_ context.Context, _, _ string) ([]domain.CreditNote, error) {
	return f.notes, nil
}

// TestBuildPDFContext_OneBuilderForAllSurfaces: the emailed, downloaded,
// and hosted PDFs all call this builder now — pre-P6 the emailed PDF
// hand-rolled a thinner context (no buyer address/tax id, no credit
// notes) and no surface carried the buyer's tax registration at all.
func TestBuildPDFContext_OneBuilderForAllSurfaces(t *testing.T) {
	customers := &fakePDFCustomers{
		cust: domain.Customer{DisplayName: "Acme Inc", Email: "ap@acme.test"},
		bp: domain.CustomerBillingProfile{
			LegalName: "Acme Incorporated", AddressLine1: "1 Way",
			City: "Berlin", Country: "DE", TaxID: "DE123456789",
		},
	}
	settings := &fakePDFSettings{ts: domain.TenantSettings{
		CompanyName: "Velox GmbH", CompanyCountry: "DE", TaxID: "DE987654321",
		Timezone: "Europe/Berlin", BrandColor: "#112233",
	}}
	creditNotes := &fakePDFCreditNotes{notes: []domain.CreditNote{
		{CreditNoteNumber: "CN-1", Status: domain.CreditNoteIssued, TotalCents: 500},
		{CreditNoteNumber: "CN-2", Status: domain.CreditNoteDraft, TotalCents: 900}, // never rendered
	}}

	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	inv := domain.Invoice{ID: "inv_1", CustomerID: "cus_1", BillingPeriodStart: start, BillingPeriodEnd: end}

	bt, ci, cns := BuildPDFContext(context.Background(), customers, settings, creditNotes, "t1", &inv)

	if bt.Name != "Acme Incorporated" {
		t.Errorf("legal name precedence: got %q", bt.Name)
	}
	if bt.Email != "ap@acme.test" {
		t.Errorf("email tracks customers.email: got %q", bt.Email)
	}
	if bt.TaxID != "DE123456789" {
		t.Errorf("buyer tax id: got %q, want the billing profile's (EU B2B invoices require it)", bt.TaxID)
	}
	if bt.AddressLine1 != "1 Way" || bt.Country != "DE" {
		t.Errorf("buyer address: got %+v", bt)
	}
	if ci.Name != "Velox GmbH" || ci.TaxID != "DE987654321" || ci.BrandColor != "#112233" {
		t.Errorf("company info: got %+v", ci)
	}
	if len(cns) != 1 || cns[0].Number != "CN-1" {
		t.Errorf("credit notes: got %+v, want only the ISSUED one", cns)
	}
	if inv.BillingPeriodDisplay == "" {
		t.Error("BillingPeriodDisplay not stamped for a bare-fetched invoice")
	}

	// A display already set by the service read decorator is preserved.
	inv2 := domain.Invoice{ID: "inv_2", CustomerID: "cus_1", BillingPeriodStart: start, BillingPeriodEnd: end, BillingPeriodDisplay: "preset"}
	_, _, _ = BuildPDFContext(context.Background(), customers, settings, nil, "t1", &inv2)
	if inv2.BillingPeriodDisplay != "preset" {
		t.Errorf("BillingPeriodDisplay overwritten: got %q", inv2.BillingPeriodDisplay)
	}

	// Nil deps degrade gracefully (thin context, never a panic).
	inv3 := domain.Invoice{ID: "inv_3", CustomerID: "cus_9"}
	bt3, ci3, cns3 := BuildPDFContext(context.Background(), nil, nil, nil, "t1", &inv3)
	if bt3.Name != "cus_9" || ci3.Name != "" || cns3 != nil {
		t.Errorf("nil-dep context: got %+v %+v %+v", bt3, ci3, cns3)
	}
}

// TestBuildLineItem_OverflowAndLineType: quantity × unit price can wrap
// int64 negative and PERSIST a corrupted money document; unknown
// line_type values used to surface as raw DB CHECK violations (500s).
//
// Mutation-verify: remove the division check — the overflow case
// passes validation and this test fails.
func TestBuildLineItem_OverflowAndLineType(t *testing.T) {
	if _, err := buildLineItem(AddLineItemInput{
		Description: "big", Quantity: 3, UnitAmountCents: math.MaxInt64 / 2,
	}, "USD"); err == nil {
		t.Error("overflowing quantity × unit accepted; want 422")
	}
	// Boundary: exactly representable product passes.
	li, err := buildLineItem(AddLineItemInput{
		Description: "max", Quantity: 1, UnitAmountCents: math.MaxInt64,
	}, "USD")
	if err != nil {
		t.Errorf("boundary product rejected: %v", err)
	} else if li.AmountCents != math.MaxInt64 {
		t.Errorf("boundary amount: got %d", li.AmountCents)
	}
	if _, err := buildLineItem(AddLineItemInput{
		Description: "weird", Quantity: 1, UnitAmountCents: 100, LineType: "manual",
	}, "USD"); err == nil {
		t.Error("unknown line_type accepted; want 422 (DB CHECK would 500)")
	}
}

type capturingEmailSender struct {
	amountCents int64
	called      bool
}

func (c *capturingEmailSender) SendInvoice(_ context.Context, _, _ string, _ []string, _, _ string, totalCents int64, _ string, _ []byte, _ string) error {
	c.called = true
	c.amountCents = totalCents
	return nil
}

// TestSendEmail_StatesAmountDueNotTotal: the email template labels the
// figure "Amount due" — under credits/partial payments that is NOT the
// invoice total. Negative case: a partially-settled invoice must email
// the residual, not the pre-credit total.
//
// Mutation-verify: pass inv.TotalAmountCents again — this test fails.
func TestSendEmail_StatesAmountDueNotTotal(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	inv, err := store.Create(ctx, "t1", domain.Invoice{
		InvoiceNumber: "INV-042", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentPending, CustomerID: "cus_1", Currency: "USD",
		TotalAmountCents: 10000, AmountDueCents: 4000, // $60 credit applied
	})
	if err != nil {
		t.Fatalf("seed invoice: %v", err)
	}

	sender := &capturingEmailSender{}
	h := &Handler{svc: NewService(store, nil, nil), emailConfigured: true}
	h.emailSender = sender

	body := bytes.NewBufferString(`{"email":"ap@acme.test"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/invoices/"+inv.ID+"/send", body)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", inv.ID)
	reqCtx := context.WithValue(req.Context(), auth.TestTenantIDKey(), "t1")
	reqCtx = context.WithValue(reqCtx, chi.RouteCtxKey, rctx)
	req = req.WithContext(reqCtx)
	rr := httptest.NewRecorder()

	h.sendEmail(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("sendEmail status: %d body=%s", rr.Code, rr.Body.String())
	}
	if !sender.called {
		t.Fatal("email sender not called")
	}
	if sender.amountCents != 4000 {
		t.Errorf("emailed amount: got %d, want 4000 (AmountDue after credits — 10000 is the pre-credit total)", sender.amountCents)
	}

	var resp map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
}
