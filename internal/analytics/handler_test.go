package analytics

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/auth"
)

// ---------------------------------------------------------------------------
// Route registration
// ---------------------------------------------------------------------------

func TestRoutes_RegistersExpectedEndpoints(t *testing.T) {
	h := NewHandler(nil)
	r := h.Routes()

	chiRouter, ok := r.(*chi.Mux)
	if !ok {
		t.Fatal("Routes() should return a *chi.Mux")
	}

	type route struct{ method, pattern string }
	var routes []route
	_ = chi.Walk(chiRouter, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routes = append(routes, route{method, pattern})
		return nil
	})

	expected := []route{
		{"GET", "/overview"},
		{"GET", "/revenue-chart"},
		{"GET", "/usage"},
	}

	for _, want := range expected {
		found := false
		for _, got := range routes {
			if got.method == want.method && got.pattern == want.pattern {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing route: %s %s", want.method, want.pattern)
		}
	}
}

func TestRoutes_MethodNotAllowed(t *testing.T) {
	h := NewHandler(nil)
	r := h.Routes()

	req := httptest.NewRequest("POST", "/overview", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /overview: got %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

// ---------------------------------------------------------------------------
// Response shapes
// ---------------------------------------------------------------------------

func TestOverviewResponse_JSONShape(t *testing.T) {
	resp := OverviewResponse{
		Period:        "30d",
		Currency:      "USD",
		Revenue:       500_000_00,
		RevenuePrev:   450_000_00,
		OutstandingAR: 15_000_00,
		OutstandingByCurrency: []CurrencyAmount{
			{Currency: "USD", AmountCents: 15_000_00},
		},
		CreditBalanceByCurrency: []CurrencyAmount{
			{Currency: "USD", AmountCents: 8_500_00},
		},
		CreditSoldByCurrency:  []CurrencyAmount{{Currency: "USD", AmountCents: 10_000_00}},
		UsageBilledByCurrency: []CurrencyAmount{{Currency: "USD", AmountCents: 2_000_00}},
		ActiveCustomers:       42,
		NewCustomers:          5,
		PaidInvoices:          156,
		FailedPayments:        3,
		OpenInvoices:          12,
		DunningActive:         2,
		UsageEvents:           12_345,
		CustomersOutOfCredit:  1,
		CustomersLowOnCredit:  2,
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal OverviewResponse: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	expected := []string{
		"period", "currency",
		"revenue", "revenue_prev", "outstanding_ar", "outstanding_by_currency", "credit_balance_by_currency",
		"credit_sold_by_currency", "usage_billed_by_currency",
		"active_customers", "new_customers",
		"paid_invoices", "failed_payments", "failed_payments_prev", "open_invoices",
		"dunning_active", "refunds_needing_attention", "usage_events",
		"customers_out_of_credit", "customers_low_on_credit",
	}
	for _, field := range expected {
		if _, ok := m[field]; !ok {
			t.Errorf("missing JSON field %q", field)
		}
	}
	if len(m) != len(expected) {
		t.Errorf("OverviewResponse has %d fields, want %d", len(m), len(expected))
	}
}

func TestOverviewResponse_ZeroValues(t *testing.T) {
	var resp OverviewResponse
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var m map[string]any
	_ = json.Unmarshal(data, &m)

	if m["period"] != "" {
		t.Errorf("period: got %v, want empty", m["period"])
	}
	if m["revenue"] != float64(0) {
		t.Errorf("revenue: got %v, want 0", m["revenue"])
	}
}

func TestRevenueDataPoint_JSONShape(t *testing.T) {
	dp := RevenueDataPoint{
		Date:         "2026-04-01",
		RevenueCents: 150_000,
		InvoiceCount: 42,
	}

	data, err := json.Marshal(dp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var m map[string]any
	_ = json.Unmarshal(data, &m)

	if m["date"] != "2026-04-01" {
		t.Errorf("date: got %v", m["date"])
	}
	if m["revenue_cents"] != float64(150_000) {
		t.Errorf("revenue_cents: got %v", m["revenue_cents"])
	}
	if m["invoice_count"] != float64(42) {
		t.Errorf("invoice_count: got %v", m["invoice_count"])
	}
}

func TestUsagePoint_JSONShape(t *testing.T) {
	p := UsagePoint{Date: "2026-04-01", Events: 100, Quantity: decimal.NewFromInt(5000)}
	data, _ := json.Marshal(p)
	var m map[string]any
	_ = json.Unmarshal(data, &m)

	for _, k := range []string{"date", "events", "quantity"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing field %q", k)
		}
	}
}

// ---------------------------------------------------------------------------
// Period parsing
// ---------------------------------------------------------------------------

func TestParsePeriod(t *testing.T) {
	tests := []struct {
		key       string
		wantKey   string
		wantDays  int
		wantTrunc string
	}{
		{"", "30d", 30, "day"},
		{"7d", "7d", 7, "day"},
		{"30d", "30d", 30, "day"},
		{"90d", "90d", 90, "day"},
		{"12m", "12m", 365, "month"},
		{"bogus", "30d", 30, "day"},
	}

	for _, tt := range tests {
		t.Run("period="+tt.key, func(t *testing.T) {
			p := parsePeriod(tt.key)
			if p.Key != tt.wantKey {
				t.Errorf("Key: got %q, want %q", p.Key, tt.wantKey)
			}
			if p.Trunc != tt.wantTrunc {
				t.Errorf("Trunc: got %q, want %q", p.Trunc, tt.wantTrunc)
			}

			// Window is close to wantDays in length, accounting for rounding.
			length := p.End.Sub(p.Start)
			wantLen := time.Duration(tt.wantDays) * 24 * time.Hour
			if abs(length-wantLen) > time.Hour {
				t.Errorf("length: got %v, want ~%v", length, wantLen)
			}

			// Prior window is the same length and ends where current begins.
			if !p.PrevEnd.Equal(p.Start) {
				t.Errorf("prev window does not abut current: prevEnd=%v start=%v", p.PrevEnd, p.Start)
			}
			if abs(p.PrevEnd.Sub(p.PrevStart)-length) > time.Hour {
				t.Errorf("prev window length mismatch")
			}
		})
	}
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// ---------------------------------------------------------------------------
// Constructor / context helpers
// ---------------------------------------------------------------------------

func TestNewHandler(t *testing.T) {
	h := NewHandler(nil)
	if h == nil {
		t.Fatal("NewHandler returned nil")
	}
	if h.db != nil {
		t.Error("db should be nil when passed nil")
	}
}

func TestTenantIDFromContext(t *testing.T) {
	ctx := auth.WithTenantID(context.Background(), "t_test_123")
	if got := auth.TenantID(ctx); got != "t_test_123" {
		t.Errorf("TenantID: got %q, want %q", got, "t_test_123")
	}
}

func TestTenantIDFromContext_Empty(t *testing.T) {
	if got := auth.TenantID(context.Background()); got != "" {
		t.Errorf("TenantID: got %q, want empty", got)
	}
}

// SQL correctness is validated in integration tests (requires a real DB with
// seed data), not here.
