package invoice

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// flakySettings is a New York tenant on Net 30 whose settings read fails
// from call failFrom on (0: never), as after a database blip.
type flakySettings struct {
	failFrom int
	calls    int
}

func (f *flakySettings) Get(_ context.Context, _ string) (domain.TenantSettings, error) {
	f.calls++
	if f.failFrom > 0 && f.calls >= f.failFrom {
		return domain.TenantSettings{}, errors.New("settings read: connection reset")
	}
	return domain.TenantSettings{DefaultCurrency: "USD", Timezone: "America/New_York", NetPaymentTerms: 30}, nil
}

// 2026-10-15 15:00 UTC is 11:00 in New York. Net 30 runs on the tenant's
// calendar and crosses the end of daylight saving on November 1, so it ends
// at 11:00 New York time on November 14, 16:00 UTC. Worked out in UTC it
// ends at 15:00 UTC, an hour early.
var (
	zoneTestNow = time.Date(2026, 10, 15, 15, 0, 0, 0, time.UTC)
	zoneTestDue = time.Date(2026, 11, 14, 16, 0, 0, 0, time.UTC)
)

// Create reads the tenant's settings once and works out the due date in the
// zone it read. A second read that fails cannot move it to UTC.
func TestCreate_DueDateInTheZoneItRead_ASecondReadCannotFail(t *testing.T) {
	settings := &flakySettings{failFrom: 2}
	svc := NewService(newMemStore(), clock.NewFake(zoneTestNow), newMemNumberer().withSettings(settings))
	inv, err := svc.Create(context.Background(), "t1", CreateInput{CustomerID: "cus_1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if inv.DueAt == nil || !inv.DueAt.Equal(zoneTestDue) {
		t.Errorf("due_at = %v, want %v: Net 30 on the New York calendar", inv.DueAt, zoneTestDue)
	}
	if settings.calls != 1 {
		t.Errorf("settings reads = %d, want 1", settings.calls)
	}
}

// An operator's draft from before invoices carried a zone is finalized when
// the tenant's settings cannot be read. The finalize fails, and can be
// retried, instead of working out the due date in UTC.
func TestFinalize_UnzonedDraft_SettingsUnreadable_Refused(t *testing.T) {
	settings := &flakySettings{}
	store := newMemStore()
	svc := NewService(store, clock.NewFake(zoneTestNow), newMemNumberer().withSettings(settings))
	ctx := context.Background()
	inv, err := svc.Create(ctx, "t1", CreateInput{
		CustomerID: "cus_1",
		LineItems:  []AddLineItemInput{{Description: "Consulting", Quantity: 1, UnitAmountCents: 10000}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	legacy := store.invoices[inv.ID]
	legacy.BillingTimezone = ""
	store.invoices[inv.ID] = legacy
	settings.failFrom = settings.calls + 1

	if final, err := svc.Finalize(ctx, "t1", inv.ID); err == nil {
		t.Fatalf("finalize succeeded with due_at %v while the tenant's zone could not be read; want an error", final.DueAt)
	}
	if got := store.invoices[inv.ID]; got.Status != domain.InvoiceDraft {
		t.Errorf("status = %s, want draft", got.Status)
	}
}
