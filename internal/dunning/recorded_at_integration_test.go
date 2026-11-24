package dunning_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/dunning"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// A dunning event carries two stamps: created_at, the instant its work
// bound, and recorded_at, the database's time of the INSERT. A bound instant
// set apart from real time tells the two apart.
func TestDunningEvent_RecordedAt_DualStamp(t *testing.T) {
	db := testutil.SetupTestDB(t)
	base := postgres.WithLivemode(context.Background(), false)

	tenantID := testutil.CreateTestTenant(t, db, "Dunning recorded_at")
	custStore := customer.NewPostgresStore(db)
	invStore := invoice.NewPostgresStore(db)
	dunStore := dunning.NewPostgresStore(db)

	cust, err := custStore.Create(base, tenantID, domain.Customer{ExternalID: "dun-rec", DisplayName: "Dun Rec"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	now := time.Now().UTC()
	inv, err := invStore.CreateWithLineItems(base, tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "HC-DUNREC-1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed,
		Currency: "USD", SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
		BillingPeriodStart: now.Add(-30 * 24 * time.Hour), BillingPeriodEnd: now,
		IssuedAt: &now,
	}, nil)
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	policy, err := dunStore.UpsertPolicy(base, tenantID, domain.DunningPolicy{
		Name: "default", Enabled: true, RetryDays: []int{3, 3, 3},
		FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionMarkUncollectible,
	})
	if err != nil {
		t.Fatalf("upsert policy: %v", err)
	}

	at := time.Date(2027, 3, 7, 9, 0, 0, 0, time.UTC)
	ctx := clock.WithEffectiveNow(base, at)

	run, err := dunStore.CreateRun(ctx, tenantID, domain.InvoiceDunningRun{
		InvoiceID: inv.ID, CustomerID: cust.ID, PolicyID: policy.ID,
		State: domain.DunningActive, AttemptCount: 0,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// CreateEvent's contract: zero CreatedAt falls back to clock.Now(ctx),
	// the bound instant.
	evt, err := dunStore.CreateEvent(ctx, tenantID, domain.InvoiceDunningEvent{
		RunID: run.ID, InvoiceID: inv.ID,
		EventType: domain.DunningEventStarted, State: domain.DunningActive,
	})
	if err != nil {
		t.Fatalf("create event: %v", err)
	}
	if !evt.CreatedAt.Equal(at) {
		t.Errorf("created_at = %s, want the bound instant %s", evt.CreatedAt, at)
	}

	events, err := dunStore.ListEvents(base, tenantID, run.ID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("no events listed")
	}
	got := events[len(events)-1]
	if got.RecordedAt == nil {
		t.Fatal("recorded_at is nil; want the INSERT's time")
	}
	if got.RecordedAt.Equal(at) {
		t.Error("recorded_at = the bound instant; want the INSERT's time")
	}
	if d := time.Since(*got.RecordedAt); d < 0 || d > 2*time.Minute {
		t.Errorf("recorded_at = %s, not within the test's real-time window (drift %s)", got.RecordedAt, d)
	}
}
