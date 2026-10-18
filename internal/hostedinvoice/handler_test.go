package hostedinvoice

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// ---- In-memory fakes ----
//
// Narrow in-memory fakes for the handler's dependencies.

type fakeInvoices struct {
	byToken   map[string]domain.Invoice // token → invoice
	lineItems map[string][]domain.InvoiceLineItem
}

func newFakeInvoices() *fakeInvoices {
	return &fakeInvoices{
		byToken:   make(map[string]domain.Invoice),
		lineItems: make(map[string][]domain.InvoiceLineItem),
	}
}

func (f *fakeInvoices) GetByPublicToken(_ context.Context, token string) (domain.Invoice, bool, error) {
	inv, ok := f.byToken[token]
	if !ok {
		return domain.Invoice{}, false, errs.ErrNotFound
	}
	return inv, false, nil
}

func (f *fakeInvoices) GetWithLineItems(_ context.Context, tenantID, id string) (domain.Invoice, []domain.InvoiceLineItem, error) {
	for _, inv := range f.byToken {
		if inv.TenantID == tenantID && inv.ID == id {
			return inv, f.lineItems[id], nil
		}
	}
	return domain.Invoice{}, nil, errs.ErrNotFound
}

type fakeCustomers struct {
	customers map[string]domain.Customer
	profiles  map[string]domain.CustomerBillingProfile
}

func newFakeCustomers() *fakeCustomers {
	return &fakeCustomers{
		customers: make(map[string]domain.Customer),
		profiles:  make(map[string]domain.CustomerBillingProfile),
	}
}

func (f *fakeCustomers) Get(_ context.Context, tenantID, id string) (domain.Customer, error) {
	c, ok := f.customers[id]
	if !ok || c.TenantID != tenantID {
		return domain.Customer{}, errs.ErrNotFound
	}
	return c, nil
}

func (f *fakeCustomers) GetBillingProfile(_ context.Context, tenantID, customerID string) (domain.CustomerBillingProfile, error) {
	p, ok := f.profiles[customerID]
	if !ok || p.TenantID != tenantID {
		return domain.CustomerBillingProfile{}, errs.ErrNotFound
	}
	return p, nil
}

type fakeSettings struct {
	settings map[string]domain.TenantSettings
}

func newFakeSettings() *fakeSettings {
	return &fakeSettings{settings: make(map[string]domain.TenantSettings)}
}

func (f *fakeSettings) Get(_ context.Context, tenantID string) (domain.TenantSettings, error) {
	ts, ok := f.settings[tenantID]
	if !ok {
		return domain.TenantSettings{}, errs.ErrNotFound
	}
	return ts, nil
}

type fakeCreditNotes struct{}

func (fakeCreditNotes) List(_ context.Context, _, _ string) ([]domain.CreditNote, error) {
	return nil, nil
}

type fakeCheckout struct {
	lastInvoice domain.Invoice
	lastSuccess string
	lastCancel  string
	lastTenant  string
	err         error
	url         string
}

func (f *fakeCheckout) CreateInvoicePaymentSession(_ context.Context, tenantID string, inv domain.Invoice, successURL, cancelURL string) (string, error) {
	f.lastInvoice = inv
	f.lastSuccess = successURL
	f.lastCancel = cancelURL
	f.lastTenant = tenantID
	if f.err != nil {
		return "", f.err
	}
	if f.url == "" {
		return "https://checkout.stripe.com/c/test_123", nil
	}
	return f.url, nil
}

// ---- Fixtures ----

func seedFinalized(t *testing.T, fi *fakeInvoices, fc *fakeCustomers, fs *fakeSettings, token string) domain.Invoice {
	t.Helper()
	now := time.Now().UTC()
	inv := domain.Invoice{
		ID:               "vlx_inv_1",
		TenantID:         "t_1",
		CustomerID:       "cus_1",
		SubscriptionID:   "sub_1",
		InvoiceNumber:    "INV-2026-0001",
		Status:           domain.InvoiceFinalized,
		PaymentStatus:    domain.PaymentPending,
		Currency:         "USD",
		SubtotalCents:    10000,
		TaxFacts:         domain.TaxFacts{TaxAmountCents: 1620},
		TotalAmountCents: 11620,
		AmountDueCents:   11620,
		IssuedAt:         &now,
		PublicToken:      token,
	}
	fi.byToken[token] = inv
	fi.lineItems[inv.ID] = []domain.InvoiceLineItem{
		{
			ID:               "li_1",
			InvoiceID:        inv.ID,
			TenantID:         "t_1",
			Description:      "Pro plan — Apr 2026",
			Quantity:         1,
			UnitAmountCents:  10000,
			AmountCents:      10000,
			TotalAmountCents: 11620,
			Currency:         "USD",
		},
	}
	fc.customers["cus_1"] = domain.Customer{
		ID:          "cus_1",
		TenantID:    "t_1",
		DisplayName: "Acme Corp",
		Email:       "billing@acme.com",
	}
	fc.profiles["cus_1"] = domain.CustomerBillingProfile{
		TenantID:     "t_1",
		CustomerID:   "cus_1",
		LegalName:    "Acme Corporation, Inc.",
		AddressLine1: "123 Main St",
		City:         "San Francisco",
		State:        "CA",
		PostalCode:   "94103",
		Country:      "US",
	}
	fs.settings["t_1"] = domain.TenantSettings{
		TenantID:            "t_1",
		CompanyName:         "YourCo",
		CompanyEmail:        "billing@yourco.com",
		CompanyAddressLine1: "456 Corporate Ave",
		CompanyCity:         "Austin",
		CompanyState:        "TX",
		CompanyPostalCode:   "78701",
		CompanyCountry:      "US",
		LogoURL:             "https://cdn.yourco.com/logo.png",
		BrandColor:          "#1f6feb",
		SupportURL:          "https://yourco.com/support",
	}
	return inv
}

func newTestHandler(fi InvoiceResolver, fc CustomerGetter, fs SettingsGetter, stripe CheckoutSessionCreator) *Handler {
	return New(Deps{
		Invoices:    fi,
		Customers:   fc,
		Settings:    fs,
		CreditNotes: fakeCreditNotes{},
		Stripe:      stripe,
		BaseURL:     "https://app.velox.dev",
	})
}

func mountRouter(h *Handler) *chi.Mux {
	r := chi.NewRouter()
	r.Mount("/", h.Routes())
	return r
}

// ---- Tests ----

func TestViewInvoice_UnknownToken_404(t *testing.T) {
	fi := newFakeInvoices()
	h := newTestHandler(fi, newFakeCustomers(), newFakeSettings(), &fakeCheckout{})
	r := mountRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/does_not_exist", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}

func TestViewInvoice_DraftHidden_404(t *testing.T) {
	fi := newFakeInvoices()
	// Defense-in-depth: even if the DB somehow carries a draft with a
	// token (future regression), the public route must not leak it.
	fi.byToken["vlx_pinv_draft"] = domain.Invoice{
		ID:          "vlx_inv_d",
		TenantID:    "t_1",
		Status:      domain.InvoiceDraft,
		PublicToken: "vlx_pinv_draft",
	}
	h := newTestHandler(fi, newFakeCustomers(), newFakeSettings(), &fakeCheckout{})
	r := mountRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/vlx_pinv_draft", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}

func TestViewInvoice_Finalized_OK(t *testing.T) {
	fi := newFakeInvoices()
	fc := newFakeCustomers()
	fs := newFakeSettings()
	seedFinalized(t, fi, fc, fs, "vlx_pinv_ok")

	h := newTestHandler(fi, fc, fs, &fakeCheckout{})
	r := mountRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/vlx_pinv_ok", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp hostedInvoicePayload
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.Invoice.InvoiceNumber != "INV-2026-0001" {
		t.Errorf("invoice_number = %q", resp.Invoice.InvoiceNumber)
	}
	if resp.Invoice.TotalAmountCents != 11620 {
		t.Errorf("total = %d, want 11620", resp.Invoice.TotalAmountCents)
	}
	if !resp.PayEnabled {
		t.Error("pay_enabled should be true for finalized+amount_due>0")
	}
	if resp.Branding.CompanyName != "YourCo" {
		t.Errorf("company_name = %q", resp.Branding.CompanyName)
	}
	if resp.Branding.BrandColor != "#1f6feb" {
		t.Errorf("brand_color = %q", resp.Branding.BrandColor)
	}
	if resp.Branding.LogoURL == "" {
		t.Error("logo_url should be present")
	}
	if resp.BillTo.Name != "Acme Corporation, Inc." {
		t.Errorf("bill_to.name = %q — expected profile legal_name override", resp.BillTo.Name)
	}
	if len(resp.LineItems) != 1 {
		t.Fatalf("line_items = %d, want 1", len(resp.LineItems))
	}
	// Safe-projection audit: no tenant_id, stripe_payment_intent_id, or
	// tax_transaction_id should surface in the serialized payload.
	raw := w.Body.String()
	forbidden := []string{"tenant_id", "stripe_payment_intent_id", "tax_transaction_id", "subscription_id", "tax_id"}
	for _, f := range forbidden {
		if containsJSONField(raw, f) {
			t.Errorf("safe-projection leak: response contains %q", f)
		}
	}
}

func TestViewInvoice_Paid_PayDisabled(t *testing.T) {
	fi := newFakeInvoices()
	fc := newFakeCustomers()
	fs := newFakeSettings()
	inv := seedFinalized(t, fi, fc, fs, "vlx_pinv_paid")
	paidAt := time.Now().UTC()
	inv.Status = domain.InvoicePaid
	inv.PaymentStatus = domain.PaymentSucceeded
	inv.AmountDueCents = 0
	inv.AmountPaidCents = 11620
	inv.PaidAt = &paidAt
	fi.byToken["vlx_pinv_paid"] = inv

	h := newTestHandler(fi, fc, fs, &fakeCheckout{})
	r := mountRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/vlx_pinv_paid", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("paid invoice should still be viewable, got %d", w.Code)
	}
	var resp hostedInvoicePayload
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.PayEnabled {
		t.Error("pay_enabled must be false for paid invoices")
	}
	if resp.Invoice.PaidAt == nil {
		t.Error("paid_at should be present")
	}
}

func TestViewInvoice_Voided_PayDisabled(t *testing.T) {
	fi := newFakeInvoices()
	fc := newFakeCustomers()
	fs := newFakeSettings()
	inv := seedFinalized(t, fi, fc, fs, "vlx_pinv_void")
	voidedAt := time.Now().UTC()
	inv.Status = domain.InvoiceVoided
	inv.VoidedAt = &voidedAt
	fi.byToken["vlx_pinv_void"] = inv

	h := newTestHandler(fi, fc, fs, &fakeCheckout{})
	r := mountRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/vlx_pinv_void", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("voided invoice should still be viewable, got %d", w.Code)
	}
	var resp hostedInvoicePayload
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.PayEnabled {
		t.Error("pay_enabled must be false for voided invoices")
	}
}

func TestCheckout_Finalized_ReturnsURL(t *testing.T) {
	fi := newFakeInvoices()
	fc := newFakeCustomers()
	fs := newFakeSettings()
	seedFinalized(t, fi, fc, fs, "vlx_pinv_pay")

	stripe := &fakeCheckout{url: "https://checkout.stripe.com/c/cs_test_abc"}
	h := newTestHandler(fi, fc, fs, stripe)
	r := mountRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/vlx_pinv_pay/checkout", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.URL != "https://checkout.stripe.com/c/cs_test_abc" {
		t.Errorf("url = %q", resp.URL)
	}
	// Stripe adapter sees the right tenant, invoice, and return URLs.
	if stripe.lastTenant != "t_1" {
		t.Errorf("stripe.lastTenant = %q", stripe.lastTenant)
	}
	if stripe.lastInvoice.ID != "vlx_inv_1" {
		t.Errorf("stripe.lastInvoice.ID = %q", stripe.lastInvoice.ID)
	}
	if stripe.lastSuccess != "https://app.velox.dev/invoice/vlx_pinv_pay?paid=1" {
		t.Errorf("success url = %q", stripe.lastSuccess)
	}
	if stripe.lastCancel != "https://app.velox.dev/invoice/vlx_pinv_pay" {
		t.Errorf("cancel url = %q", stripe.lastCancel)
	}
}

func TestCheckout_Paid_Conflict(t *testing.T) {
	fi := newFakeInvoices()
	fc := newFakeCustomers()
	fs := newFakeSettings()
	inv := seedFinalized(t, fi, fc, fs, "vlx_pinv_paid2")
	inv.Status = domain.InvoicePaid
	inv.AmountDueCents = 0
	fi.byToken["vlx_pinv_paid2"] = inv

	h := newTestHandler(fi, fc, fs, &fakeCheckout{})
	r := mountRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/vlx_pinv_paid2/checkout", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409", w.Code)
	}
}

func TestCheckout_NoStripe_ServiceUnavailable(t *testing.T) {
	fi := newFakeInvoices()
	fc := newFakeCustomers()
	fs := newFakeSettings()
	seedFinalized(t, fi, fc, fs, "vlx_pinv_noStripe")

	// Stripe intentionally nil — dev environment without Stripe creds.
	h := newTestHandler(fi, fc, fs, nil)
	r := mountRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/vlx_pinv_noStripe/checkout", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", w.Code)
	}
}

func TestCheckout_StripeError_BadGateway(t *testing.T) {
	fi := newFakeInvoices()
	fc := newFakeCustomers()
	fs := newFakeSettings()
	seedFinalized(t, fi, fc, fs, "vlx_pinv_err")

	stripe := &fakeCheckout{err: errors.New("stripe exploded")}
	h := newTestHandler(fi, fc, fs, stripe)
	r := mountRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/vlx_pinv_err/checkout", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("got %d, want 502", w.Code)
	}
}

func TestDownloadPDF_Finalized_OK(t *testing.T) {
	fi := newFakeInvoices()
	fc := newFakeCustomers()
	fs := newFakeSettings()
	seedFinalized(t, fi, fc, fs, "vlx_pinv_pdf")

	h := newTestHandler(fi, fc, fs, &fakeCheckout{})
	r := mountRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/vlx_pinv_pdf/pdf", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/pdf" {
		t.Errorf("content-type = %q", ct)
	}
	if w.Body.Len() < 100 {
		t.Errorf("pdf body too small: %d bytes", w.Body.Len())
	}
}

func TestDownloadPDF_UnknownToken_404(t *testing.T) {
	h := newTestHandler(newFakeInvoices(), newFakeCustomers(), newFakeSettings(), &fakeCheckout{})
	r := mountRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/never_existed/pdf", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}

// containsJSONField is a trivial substring check for safe-projection
// auditing. Not a full JSON walk because the forbidden names don't appear
// in any value, only as field names; substring is sufficient here.
func containsJSONField(raw, field string) bool {
	// Match "<field>": to avoid false hits on values that happen to
	// contain the word.
	needle := "\"" + field + "\":"
	for i := 0; i+len(needle) <= len(raw); i++ {
		if raw[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestViewInvoice_FractionalQuantity_CarriesQuantityDecimal locks the
// qty × unit = amount coherence fix for the hosted page: a fractional-usage
// line (1.5 GPU-hours at $6.67/hr = $10.00) must surface quantity_decimal in
// the JSON so the SPA renders "1.5", not the truncated integer 1 against the
// full $10.00 amount. The invoice PDF already handles this via its
// QuantityDecimal-aware formatter; the hosted payload dropped the field.
func TestViewInvoice_FractionalQuantity_CarriesQuantityDecimal(t *testing.T) {
	fi := newFakeInvoices()
	fc := newFakeCustomers()
	fs := newFakeSettings()
	inv := seedFinalized(t, fi, fc, fs, "vlx_pinv_frac")
	fi.lineItems[inv.ID] = []domain.InvoiceLineItem{{
		ID:              "li_frac",
		InvoiceID:       inv.ID,
		TenantID:        "t_1",
		Description:     "GPU hours",
		Quantity:        1, // truncation of 1.5
		QuantityDecimal: decimal.RequireFromString("1.5"),
		UnitAmountCents: 667, AmountCents: 1000, TotalAmountCents: 1000,
		Currency: "USD",
	}}

	h := newTestHandler(fi, fc, fs, &fakeCheckout{})
	r := mountRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/vlx_pinv_frac", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp hostedInvoicePayload
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(resp.LineItems) != 1 {
		t.Fatalf("line_items = %d, want 1", len(resp.LineItems))
	}
	if resp.LineItems[0].QuantityDecimal != "1.5" {
		t.Errorf("quantity_decimal = %q, want \"1.5\" (SPA must render the exact fractional qty)", resp.LineItems[0].QuantityDecimal)
	}
	// Whole-quantity lines keep the field empty (omitempty) — the SPA falls
	// back to the integer quantity.
	fi.lineItems[inv.ID][0].QuantityDecimal = decimal.Decimal{}
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/vlx_pinv_frac", nil))
	var resp2 hostedInvoicePayload
	if err := json.Unmarshal(w2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode body 2: %v", err)
	}
	if resp2.LineItems[0].QuantityDecimal != "" {
		t.Errorf("whole-qty line quantity_decimal = %q, want empty", resp2.LineItems[0].QuantityDecimal)
	}
}

// TestViewInvoice_CarriesTaxExemptReason locks the projection fix: the hosted
// invoice page renders its exemption legend from tax_exempt_reason, but the
// field was dropped from viewInvoice — so an exempt ($0-tax, non-reverse-charge)
// invoice showed no disclosure on the public page while reverse-charge did.
func TestViewInvoice_CarriesTaxExemptReason(t *testing.T) {
	fi := newFakeInvoices()
	fc := newFakeCustomers()
	fs := newFakeSettings()
	inv := seedFinalized(t, fi, fc, fs, "vlx_pinv_exempt")
	// Exempt invoice: $0 tax, an operator-supplied reason, not reverse-charge.
	inv.TaxAmountCents = 0
	inv.TaxExemptReason = "Reseller — resale certificate"
	inv.TaxReverseCharge = false
	fi.byToken["vlx_pinv_exempt"] = inv

	h := newTestHandler(fi, fc, fs, &fakeCheckout{})
	r := mountRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/vlx_pinv_exempt", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp hostedInvoicePayload
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.Invoice.TaxExemptReason != "Reseller — resale certificate" {
		t.Errorf("tax_exempt_reason = %q, want the operator reason carried through to the hosted page", resp.Invoice.TaxExemptReason)
	}
}

// A charge sent to Stripe whose result is not saved (the charge record,
// ADR-127): the payment status is still pending, and only the record says a
// payment may be on its way. The Pay button hides, or the customer could pay
// a second time on the hosted page.
func TestViewInvoice_ChargeRecorded_PayDisabled(t *testing.T) {
	fi := newFakeInvoices()
	fc := newFakeCustomers()
	fs := newFakeSettings()
	inv := seedFinalized(t, fi, fc, fs, "vlx_pinv_recorded")
	started := time.Now().UTC().Add(-20 * time.Minute)
	inv.ChargeStartedAt = &started
	fi.byToken["vlx_pinv_recorded"] = inv

	h := newTestHandler(fi, fc, fs, &fakeCheckout{})
	r := mountRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/vlx_pinv_recorded", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	var resp hostedInvoicePayload
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.PayEnabled {
		t.Error("pay_enabled must be false while a charge of this invoice may be taking the money")
	}
}
