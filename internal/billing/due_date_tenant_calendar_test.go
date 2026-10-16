package billing

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// tzNetSettings is mockSettings with a tenant timezone and Net terms.
type tzNetSettings struct {
	mockSettings
	tz      string
	netDays int
}

func (s *tzNetSettings) Get(_ context.Context, _ string) (domain.TenantSettings, error) {
	return domain.TenantSettings{Timezone: s.tz, NetPaymentTerms: s.netDays}, nil
}

// A Net 30 cycle invoice for an America/New_York tenant whose period closes at
// 2026-11-01 00:00 EDT is due 2026-12-01 00:00 EST. Pre-fix the engine added
// 30×24h to the UTC close instant, landing at 2026-11-30 23:00 EST, so the PDF
// and dashboard printed "Due November 30, 2026": one day early, because the
// span crosses the end of DST on Nov 1.
func TestRunCycle_NetTermsDueDate_UsesTenantCalendarAcrossDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	periodStart := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC) // Oct 1 00:00 EDT
	periodEnd := time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC)   // Nov 1 00:00 EDT

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				Items:                     []domain.SubscriptionItem{{ID: "it_1", PlanID: "pln_1", Quantity: 1}},
				Status:                    domain.SubscriptionActive,
				BillingTime:               domain.BillingTimeCalendar,
				CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
				NextBillingAt: &periodEnd,
			},
		},
		cycleUpdated: make(map[string]bool),
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_1": {ID: "pln_1", Name: "Pro", Currency: "USD",
				BillingInterval: domain.BillingMonthly, BaseAmountCents: 4900,
				BaseBillTiming: domain.BillInArrears},
		},
	}
	invoices := &mockInvoices{}
	engine := wireBaseTax(NewEngine(subs, &mockUsage{}, pricing, invoices, nil,
		&tzNetSettings{tz: "America/New_York", netDays: 30}, nil, nil,
		clock.NewFake(periodEnd.Add(time.Second))))

	count, errs := engine.RunCycle(context.Background(), 50, nil)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if count != 1 {
		t.Fatalf("invoices generated: got %d, want 1", count)
	}
	inv := invoices.invoices[0]
	if inv.DueAt == nil {
		t.Fatal("due_at not set")
	}
	want := time.Date(2026, 12, 1, 5, 0, 0, 0, time.UTC) // Dec 1 00:00 EST
	if !inv.DueAt.Equal(want) {
		t.Errorf("due_at = %s (%s in NY), want %s (Dec 1 00:00 EST)",
			inv.DueAt.UTC().Format(time.RFC3339), inv.DueAt.In(ny).Format("Jan 2 15:04 MST"), want.Format(time.RFC3339))
	}
	if got := inv.DueAt.In(ny).Format("January 2, 2006"); got != "December 1, 2026" {
		t.Errorf("printed due date = %q, want %q", got, "December 1, 2026")
	}
}
