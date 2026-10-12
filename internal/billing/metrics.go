package billing

import (
	"context"
	"log/slog"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// Metrics computes billing KPIs from subscription and invoice data.
type Metrics struct {
	subs     MetricsSubReader
	invoices MetricsInvoiceReader
	ttfi     MetricsTTFIReader
}

type MetricsSubReader interface {
	ListActive(ctx context.Context, tenantID string) ([]domain.Subscription, error)
}

type MetricsInvoiceReader interface {
	ListRecent(ctx context.Context, tenantID string, since time.Time, limit int) ([]domain.Invoice, error)
}

// MetricsTTFIReader feeds the time-to-first-invoice telemetry on the
// per-tenant dashboard. It reads the audit log + tenants table directly
// rather than instrumenting the invoice Finalize hot path — keeps the
// invoice service free of telemetry coupling and lets backfills, deletions,
// or operator-driven re-finalizes be answered correctly without having to
// remember to re-emit a metric.
type MetricsTTFIReader interface {
	// FirstInvoiceFinalizedAt returns the timestamp of the earliest
	// invoice.finalize audit log entry for the tenant. Returns nil if
	// the tenant has never finalized an invoice.
	FirstInvoiceFinalizedAt(ctx context.Context, tenantID string) (*time.Time, error)
	// TenantCreatedAt returns the row creation timestamp for the tenant.
	TenantCreatedAt(ctx context.Context, tenantID string) (time.Time, error)
}

type Dashboard struct {
	MRRCents                  int64          `json:"mrr_cents"`
	ARRCents                  int64          `json:"arr_cents"`
	ActiveSubscriptions       int            `json:"active_subscriptions"`
	TotalCustomers            int            `json:"total_customers"`
	RevenueThisMonth          int64          `json:"revenue_this_month_cents"`
	InvoicesByStatus          map[string]int `json:"invoices_by_status"`
	GeneratedAt               time.Time      `json:"generated_at"`
	TimeToFirstInvoiceSeconds *float64       `json:"time_to_first_invoice_seconds,omitempty"`
}

func NewMetrics(subs MetricsSubReader, invoices MetricsInvoiceReader, ttfi MetricsTTFIReader) *Metrics {
	return &Metrics{subs: subs, invoices: invoices, ttfi: ttfi}
}

// ComputeDashboard calculates MRR, ARR, and billing KPIs for a tenant.
func (m *Metrics) ComputeDashboard(ctx context.Context, tenantID string, plans map[string]domain.Plan) (Dashboard, error) {
	d := Dashboard{
		InvoicesByStatus: make(map[string]int),
		GeneratedAt:      time.Now().UTC(), // wall-clock: when this report was computed
	}

	// Active subscriptions → MRR
	activeSubs, err := m.subs.ListActive(ctx, tenantID)
	if err != nil {
		return d, err
	}
	d.ActiveSubscriptions = len(activeSubs)

	// MRR is summed per item (plan × quantity) across the active set. Items
	// sharing one subscription contribute independently — a 2-seat Pro plan +
	// 10-seat Add-on plan rolls up as (pro.base × 2) + (addon.base × 10).
	// Yearly-billing items are normalized by dividing base / 12.
	customerSet := make(map[string]bool)
	for _, sub := range activeSubs {
		customerSet[sub.CustomerID] = true

		for _, it := range sub.Items {
			plan, ok := plans[it.PlanID]
			if !ok {
				continue
			}

			// Multiply by quantity BEFORE dividing by 12 so the yearly
			// normalization doesn't truncate per-unit then scale the error
			// (e.g. $100/yr × 10 = 8333¢/mo, not (10000/12)×10 = 8330¢).
			if plan.BillingInterval != domain.BillingYearly && plan.BillingInterval != domain.BillingMonthly {
				continue
			}
			lineMRR := plan.BaseAmountCents * it.Quantity
			if plan.BillingInterval == domain.BillingYearly {
				lineMRR /= 12
			}
			d.MRRCents += lineMRR
		}
	}
	d.TotalCustomers = len(customerSet)
	d.ARRCents = d.MRRCents * 12

	// Revenue this month
	monthStart := time.Date(time.Now().Year(), time.Now().Month(), 1, 0, 0, 0, 0, time.UTC) // wall-clock: operator dashboard KPI spans all customers, not one test clock
	recentInvoices, err := m.invoices.ListRecent(ctx, tenantID, monthStart, 1000)
	if err != nil {
		return d, err
	}

	for _, inv := range recentInvoices {
		d.InvoicesByStatus[string(inv.Status)]++
		if inv.Status == domain.InvoicePaid {
			d.RevenueThisMonth += inv.TotalAmountCents
		}
	}

	// Time-to-first-invoice — best-effort. A failure here must not break
	// the dashboard; operators rely on MRR/ARR being available even when
	// the audit log query is degraded (e.g. transient DB error, missing
	// tenant row in a partial bootstrap). We log a warning and move on.
	if m.ttfi != nil {
		finalizedAt, ferr := m.ttfi.FirstInvoiceFinalizedAt(ctx, tenantID)
		if ferr != nil {
			slog.WarnContext(ctx, "ttfi: failed to read first invoice finalized timestamp",
				"tenant_id", tenantID, "error", ferr)
		} else if finalizedAt != nil {
			createdAt, terr := m.ttfi.TenantCreatedAt(ctx, tenantID)
			if terr != nil {
				slog.WarnContext(ctx, "ttfi: failed to read tenant created_at",
					"tenant_id", tenantID, "error", terr)
			} else {
				secs := finalizedAt.Sub(createdAt).Seconds()
				d.TimeToFirstInvoiceSeconds = &secs
			}
		}
	}

	return d, nil
}
