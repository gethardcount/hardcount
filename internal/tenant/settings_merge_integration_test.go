package tenant

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

func settingsRequest(t *testing.T, tenantID, body string) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/v1/settings", bytes.NewBufferString(body))
	reqCtx := context.WithValue(req.Context(), auth.TestTenantIDKey(), tenantID)
	reqCtx = postgres.WithLivemode(reqCtx, false)
	req = req.WithContext(reqCtx)
	return httptest.NewRecorder(), req
}

// TestSettingsUpsert_MergePatch (P8): a partial body must only touch the
// fields it carries. The old handler decoded into a zero struct — an SDK
// call updating one field silently wiped the company address, invoice
// prefix, and net terms, and validateSettings re-defaulted the wreckage
// (settings looked "reset", not corrupted, so nobody noticed).
//
// Mutation-verify: decode into a zero struct again — the preserved-field
// assertions fail.
func TestSettingsUpsert_MergePatch(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Settings Merge")
	store := NewSettingsStore(db)
	h := NewSettingsHandler(store)

	// Seed a fully-configured tenant on top of the synthesized defaults
	// (a bare struct violates the enum CHECKs on tax fields).
	seed, err := store.Get(ctx, tenantID)
	if err != nil {
		t.Fatalf("get defaults: %v", err)
	}
	seed.CompanyName = "Acme GmbH"
	seed.DefaultCurrency = "EUR"
	seed.Timezone = "Europe/Berlin"
	seed.InvoicePrefix = "ACME"
	seed.NetPaymentTerms = 15
	if _, err := store.Upsert(ctx, seed); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	// Partial update: ONLY the tax name.
	rr, req := settingsRequest(t, tenantID, `{"tax_name":"VAT"}`)
	h.upsert(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("upsert status: %d body=%s", rr.Code, rr.Body.String())
	}

	got, err := store.Get(ctx, tenantID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TaxName != "VAT" {
		t.Errorf("tax_name: got %q, want VAT (the sent field)", got.TaxName)
	}
	if got.CompanyName != "Acme GmbH" || got.DefaultCurrency != "EUR" ||
		got.Timezone != "Europe/Berlin" || got.InvoicePrefix != "ACME" || got.NetPaymentTerms != 15 {
		t.Errorf("unsent fields wiped by partial update: %+v", got)
	}

	// Explicitly-present values still SET — including net terms 0 (due
	// immediately), which the old <=0 coercion made inexpressible.
	rr, req = settingsRequest(t, tenantID, `{"net_payment_terms":0}`)
	h.upsert(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("net-0 upsert status: %d body=%s", rr.Code, rr.Body.String())
	}
	got, _ = store.Get(ctx, tenantID)
	if got.NetPaymentTerms != 0 {
		t.Errorf("net_payment_terms: got %d, want 0 (Net 0 is a legal term)", got.NetPaymentTerms)
	}
	if got.CompanyName != "Acme GmbH" {
		t.Errorf("company_name wiped by net-terms update: %q", got.CompanyName)
	}

	// Explicit empty string resets a defaultable field (documented merge
	// semantics: absent = keep, present = set, "" = back to default).
	rr, req = settingsRequest(t, tenantID, `{"invoice_prefix":""}`)
	h.upsert(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("prefix-reset status: %d body=%s", rr.Code, rr.Body.String())
	}
	got, _ = store.Get(ctx, tenantID)
	if got.InvoicePrefix != "HC" {
		t.Errorf("invoice_prefix after explicit \"\": got %q, want the HC default", got.InvoicePrefix)
	}
}

// TestSettingsUpsert_ValidationRejects (P8): invalid TZ / currency /
// prefix / net-terms 422 at save time — the timezone previously saved
// unvalidated and detonated at cycle close (ADR-058 date math), far
// from the operator's edit.
func TestSettingsUpsert_ValidationRejects(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Settings Validation")
	store := NewSettingsStore(db)
	h := NewSettingsHandler(store)

	for name, body := range map[string]string{
		"invalid timezone":      `{"timezone":"Mars/Olympus_Mons"}`,
		"Local timezone":        `{"timezone":"Local"}`,
		"invalid currency":      `{"default_currency":"DOGE"}`,
		"prefix with slash":     `{"invoice_prefix":"INV/26"}`,
		"prefix with space":     `{"invoice_prefix":"IN V"}`,
		"negative net terms":    `{"net_payment_terms":-1}`,
		"net terms over a year": `{"net_payment_terms":366}`,
	} {
		rr, req := settingsRequest(t, tenantID, body)
		h.upsert(rr, req)
		if rr.Code < 400 || rr.Code >= 500 {
			if rr.Code == http.StatusOK {
				t.Errorf("%s: saved fine (200); want 4xx", name)
			} else {
				t.Errorf("%s: got %d; want 4xx", name, rr.Code)
			}
		}
	}

	// Lowercase currency is accepted and canonicalized UPPERCASE — a
	// stored "usd" breaks currency-equality filters downstream.
	rr, req := settingsRequest(t, tenantID, `{"default_currency":"usd"}`)
	h.upsert(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("lowercase currency status: %d body=%s", rr.Code, rr.Body.String())
	}
	got, err := store.Get(ctx, tenantID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.DefaultCurrency != "USD" {
		t.Errorf("default_currency: got %q, want USD (canonical uppercase)", got.DefaultCurrency)
	}

	// Valid IANA zone saves.
	rr, req = settingsRequest(t, tenantID, `{"timezone":"Asia/Kolkata"}`)
	h.upsert(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("valid timezone status: %d body=%s", rr.Code, rr.Body.String())
	}

	var resp map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
}
