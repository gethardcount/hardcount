package invoice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
)

// currencyFactsFake is CustomerCurrencyFacts: a profile currency and the plan
// currencies of live subscriptions, or a read error.
type currencyFactsFake struct {
	profile string
	plans   []string
	err     error
}

func (f currencyFactsFake) CustomerCurrencyFacts(context.Context, string, string) (string, []string, error) {
	return f.profile, f.plans, f.err
}

type currencySettingsFake struct {
	s   domain.TenantSettings
	err error
}

func (f currencySettingsFake) Get(context.Context, string) (domain.TenantSettings, error) {
	return f.s, f.err
}

// currencyHandler is the create route over a service whose currency facts
// and workspace default are the given ones.
func currencyHandler(store *memStore, facts currencyFactsFake, settings currencySettingsFake) *Handler {
	svc := NewService(store, nil, newMemNumberer())
	svc.SetCustomerCurrencyFacts(facts)
	svc.SetTenantSettingsReader(settings)
	return &Handler{svc: svc}
}

func postCreate(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(body)))
	req = req.WithContext(auth.WithTenantID(req.Context(), "t1"))
	rr := httptest.NewRecorder()
	h.create(rr, req)
	return rr
}

// TestCreate_CurrencyIsTheCustomers pins how a manual invoice gets its
// currency (ADR-100, ADR-129). An omitted currency takes the billing profile's,
// then the live subscriptions' plan currency, then the workspace default — the
// order the engine stamps a cycle invoice. An explicit one must agree with
// both facts, or the create is refused with 409 customer_currency_conflict
// and nothing is written. A customer with neither fact may be invoiced in any
// allowed currency. Unit-level; the real-Postgres proof is
// TestInvoiceCreate_CurrencyIsTheCustomers in internal/api.
//
// Mutation-verify: drop the subscription-plan arm from resolveCurrency → the
// "subscription plan" cases go red.
func TestCreate_CurrencyIsTheCustomers(t *testing.T) {
	workspace := currencySettingsFake{s: domain.TenantSettings{DefaultCurrency: "EUR"}}
	for _, c := range []struct {
		name     string
		facts    currencyFactsFake
		body     string
		currency string // want this currency, or
		conflict string // want a 409 naming this fact
	}{
		{"no currency: the billing profile's", currencyFactsFake{profile: "GBP"}, `{"customer_id":"cus_1"}`, "GBP", ""},
		{"no currency: the subscription plan's", currencyFactsFake{plans: []string{"cad"}}, `{"customer_id":"cus_1"}`, "CAD", ""},
		{"no currency, no facts: the workspace default", currencyFactsFake{}, `{"customer_id":"cus_1"}`, "EUR", ""},
		{"explicit, agreeing with both facts", currencyFactsFake{profile: "CAD", plans: []string{"CAD"}}, `{"customer_id":"cus_1","currency":"cad"}`, "CAD", ""},
		{"explicit, no facts: any allowed currency", currencyFactsFake{}, `{"customer_id":"cus_1","currency":"chf"}`, "CHF", ""},
		{"explicit against the billing profile", currencyFactsFake{profile: "USD"}, `{"customer_id":"cus_1","currency":"EUR"}`, "", "USD (billing profile)"},
		{"explicit against the subscription plan", currencyFactsFake{plans: []string{"USD"}}, `{"customer_id":"cus_1","currency":"EUR"}`, "", "USD (subscription plan)"},
		{"no currency, profile against the plan", currencyFactsFake{profile: "EUR", plans: []string{"USD"}}, `{"customer_id":"cus_1"}`, "", "USD (subscription plan)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := newMemStore()
			rr := postCreate(t, currencyHandler(store, c.facts, workspace), c.body)
			if c.conflict != "" {
				var body struct {
					Error struct{ Code, Message string } `json:"error"`
				}
				_ = json.Unmarshal(rr.Body.Bytes(), &body)
				if rr.Code != http.StatusConflict || body.Error.Code != "customer_currency_conflict" || !strings.Contains(body.Error.Message, c.conflict) {
					t.Errorf("status %d, body %s; want 409 customer_currency_conflict naming %q", rr.Code, rr.Body.String(), c.conflict)
				}
				if n := len(store.invoices); n != 0 {
					t.Errorf("%d invoice(s) created by a refused create", n)
				}
				return
			}
			if rr.Code != http.StatusCreated {
				t.Fatalf("status %d, body %s; want 201", rr.Code, rr.Body.String())
			}
			var inv domain.Invoice
			_ = json.Unmarshal(rr.Body.Bytes(), &inv)
			if inv.Currency != c.currency {
				t.Errorf("currency = %q, want %q", inv.Currency, c.currency)
			}
		})
	}
}

// A currency read that ERRORS refuses the create and writes nothing: a
// database blip used to fall through to the tenant default or USD, minting a
// wrong-currency invoice. The facts read answers 503 (retry).
func TestCreate_CurrencyReadErrorRefuses(t *testing.T) {
	blip := errors.New("connection reset")
	for name, c := range map[string]struct {
		facts    currencyFactsFake
		settings currencySettingsFake
		status   int
	}{
		"currency facts read fails":  {currencyFactsFake{err: blip}, currencySettingsFake{s: domain.TenantSettings{DefaultCurrency: "USD"}}, http.StatusServiceUnavailable},
		"tenant settings read fails": {currencyFactsFake{}, currencySettingsFake{err: blip}, http.StatusInternalServerError},
	} {
		t.Run(name, func(t *testing.T) {
			store := newMemStore()
			rr := postCreate(t, currencyHandler(store, c.facts, c.settings), `{"customer_id":"cus_1"}`)
			if rr.Code != c.status {
				t.Fatalf("status = %d (%s), want %d", rr.Code, rr.Body.String(), c.status)
			}
			if n := len(store.invoices); n != 0 {
				t.Errorf("%d invoice(s) created; a failed currency read must create none", n)
			}
		})
	}
}
