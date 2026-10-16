package invoice

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// tzSettingsReader returns a tenant timezone with Net terms.
type tzSettingsReader struct {
	tz      string
	netDays int
}

func (f tzSettingsReader) Get(_ context.Context, _ string) (domain.TenantSettings, error) {
	return domain.TenantSettings{Timezone: f.tz, NetPaymentTerms: f.netDays}, nil
}

// A manual Net 30 invoice for an America/New_York tenant, created and then
// finalized at 2026-11-01 00:00 EDT, is due 2026-12-01 00:00 EST. Pre-fix both
// Create and Finalize added 30×24h to the UTC instant, giving 2026-11-30
// 23:00 EST, so the invoice printed "Due November 30, 2026".
func TestManualInvoice_NetTermsDueDate_UsesTenantCalendarAcrossDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	at := time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC)   // Nov 1 00:00 EDT
	want := time.Date(2026, 12, 1, 5, 0, 0, 0, time.UTC) // Dec 1 00:00 EST

	fake := clock.NewFake(at)
	svc := NewService(newMemStore(), fake, newMemNumberer())
	svc.SetTenantSettingsReader(tzSettingsReader{tz: "America/New_York", netDays: 30})
	ctx := context.Background()

	inv, err := svc.Create(ctx, "t1", CreateInput{
		CustomerID: "cus_1",
		LineItems:  []AddLineItemInput{{Description: "Consulting", Quantity: 1, UnitAmountCents: 10000}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if inv.DueAt == nil || !inv.DueAt.Equal(want) {
		t.Errorf("draft due_at = %v, want %s (Dec 1 00:00 EST)", inv.DueAt, want.Format(time.RFC3339))
	}

	final, err := svc.Finalize(ctx, "t1", inv.ID)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if final.DueAt == nil || !final.DueAt.Equal(want) {
		t.Fatalf("finalized due_at = %v, want %s (Dec 1 00:00 EST)", final.DueAt, want.Format(time.RFC3339))
	}
	if got := final.DueAt.In(ny).Format("January 2, 2006"); got != "December 1, 2026" {
		t.Errorf("printed due date = %q, want %q", got, "December 1, 2026")
	}
}
