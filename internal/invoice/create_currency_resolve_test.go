package invoice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

type currencyCustomerFake struct {
	profile domain.CustomerBillingProfile
	err     error
}

func (f currencyCustomerFake) Get(context.Context, string, string) (domain.Customer, error) {
	return domain.Customer{ID: "cus_1"}, nil
}
func (f currencyCustomerFake) GetBillingProfile(context.Context, string, string) (domain.CustomerBillingProfile, error) {
	return f.profile, f.err
}

type currencySettingsFake struct {
	s   domain.TenantSettings
	err error
}

func (f currencySettingsFake) Get(context.Context, string) (domain.TenantSettings, error) {
	return f.s, f.err
}

// TestCreate_CurrencyResolvesProfileThenTenantDefault pins the FLOW I8
// contract for one-off invoices (fixed 2026-07-26): an omitted currency
// resolves customer billing-profile → tenant default → USD. Pre-fix the
// service hardcoded USD, so a bare API create for a GBP-profile
// customer silently minted a USD invoice (the composer masked it by
// always sending its picker value). Explicit input always wins.
func TestCreate_CurrencyResolvesProfileThenTenantDefault(t *testing.T) {
	post := func(t *testing.T, h *Handler, body map[string]any) domain.Invoice {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b))
		req = req.WithContext(auth.WithTenantID(req.Context(), "t1"))
		rr := httptest.NewRecorder()
		h.create(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("status: got %d, body=%s", rr.Code, rr.Body.String())
		}
		var inv domain.Invoice
		if err := json.Unmarshal(rr.Body.Bytes(), &inv); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return inv
	}

	newHandler := func(profileCurrency, tenantDefault string) *Handler {
		h := &Handler{svc: NewService(newMemStore(), nil, newMemNumberer())}
		h.customers = currencyCustomerFake{profile: domain.CustomerBillingProfile{Currency: profileCurrency}}
		h.settings = currencySettingsFake{s: domain.TenantSettings{DefaultCurrency: tenantDefault}}
		return h
	}

	t.Run("profile currency wins over tenant default", func(t *testing.T) {
		inv := post(t, newHandler("GBP", "EUR"), map[string]any{"customer_id": "cus_1"})
		if inv.Currency != "GBP" {
			t.Errorf("currency = %q, want GBP (billing profile)", inv.Currency)
		}
	})

	t.Run("no profile currency → tenant default", func(t *testing.T) {
		inv := post(t, newHandler("", "EUR"), map[string]any{"customer_id": "cus_1"})
		if inv.Currency != "EUR" {
			t.Errorf("currency = %q, want EUR (tenant default)", inv.Currency)
		}
	})

	t.Run("neither → USD fallback", func(t *testing.T) {
		inv := post(t, newHandler("", ""), map[string]any{"customer_id": "cus_1"})
		if inv.Currency != "USD" {
			t.Errorf("currency = %q, want USD", inv.Currency)
		}
	})

	t.Run("explicit input always wins", func(t *testing.T) {
		inv := post(t, newHandler("GBP", "EUR"), map[string]any{"customer_id": "cus_1", "currency": "cad"})
		if inv.Currency != "CAD" {
			t.Errorf("currency = %q, want CAD (explicit, canonicalized)", inv.Currency)
		}
	})
}

// A currency read that ERRORS refuses the create (503, nothing created): a
// database blip used to fall through to the tenant default or USD, minting a
// wrong-currency invoice. A billing profile that does not exist is "not set"
// and still falls through to the tenant default.
func TestCreate_CurrencyReadErrorRefuses(t *testing.T) {
	try := func(t *testing.T, cust currencyCustomerFake, set currencySettingsFake) (*httptest.ResponseRecorder, *memStore) {
		t.Helper()
		store := newMemStore()
		h := &Handler{svc: NewService(store, nil, newMemNumberer()), customers: cust, settings: set}
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"customer_id":"cus_1"}`)))
		req = req.WithContext(auth.WithTenantID(req.Context(), "t1"))
		rr := httptest.NewRecorder()
		h.create(rr, req)
		return rr, store
	}
	blip := errors.New("connection reset")

	for name, c := range map[string]struct {
		cust currencyCustomerFake
		set  currencySettingsFake
	}{
		"billing profile read fails": {currencyCustomerFake{err: blip}, currencySettingsFake{s: domain.TenantSettings{DefaultCurrency: "USD"}}},
		"tenant settings read fails": {currencyCustomerFake{err: errs.ErrNotFound}, currencySettingsFake{err: blip}},
	} {
		t.Run(name, func(t *testing.T) {
			rr, store := try(t, c.cust, c.set)
			if rr.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d (%s), want 503", rr.Code, rr.Body.String())
			}
			if n := len(store.invoices); n != 0 {
				t.Errorf("%d invoice(s) created; a failed currency read must create none", n)
			}
		})
	}

	t.Run("no billing profile → tenant default", func(t *testing.T) {
		rr, _ := try(t, currencyCustomerFake{err: errs.ErrNotFound}, currencySettingsFake{s: domain.TenantSettings{DefaultCurrency: "EUR"}})
		if rr.Code != http.StatusCreated {
			t.Fatalf("status = %d (%s), want 201", rr.Code, rr.Body.String())
		}
		var inv domain.Invoice
		_ = json.Unmarshal(rr.Body.Bytes(), &inv)
		if inv.Currency != "EUR" {
			t.Errorf("currency = %q, want EUR", inv.Currency)
		}
	})
}
