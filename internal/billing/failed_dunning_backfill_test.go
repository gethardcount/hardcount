package billing

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// failedBackfillInvoice is a finalized, failed, still-owed invoice updated at the
// given instant — the dunning_backfill candidate shape (the "no run" NOT EXISTS is
// a DB concern the mock can't model; that's the real-Postgres test's job).
func failedBackfillInvoice(id string, updatedAt time.Time) domain.Invoice {
	return domain.Invoice{
		ID: id, TenantID: "t1", CustomerID: "cus_1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed,
		AmountDueCents: 5000, UpdatedAt: updatedAt,
	}
}

// TestEnrollFailedWithoutDunning_EnrollsFailedInvoice locks the backstop: a
// finalized invoice left failed with no run (the SettleFailed post-commit crash /
// exhausted-retry window) is handed to the idempotent StartDunning by the sweep.
func TestEnrollFailedWithoutDunning_EnrollsFailedInvoice(t *testing.T) {
	aged := time.Now().UTC().Add(-time.Hour) // older than the cool-off
	inv := &mockInvoices{invoices: []domain.Invoice{failedBackfillInvoice("inv_1", aged)}}
	engine := noPMEngine(t, inv)
	starter := &recordingDunningStarter{}
	engine.SetDunningStarter(starter)

	swept, errsOut := engine.EnrollFailedWithoutDunning(context.Background(), 10)
	if len(errsOut) != 0 {
		t.Fatalf("unexpected errors: %v", errsOut)
	}
	if swept != 1 {
		t.Fatalf("swept = %d, want 1", swept)
	}
	if len(starter.started) != 1 || starter.started[0] != "inv_1" {
		t.Fatalf("StartDunning calls = %v, want [inv_1]", starter.started)
	}
	// The failed-invoice backfill enrolls invoices whose charge REALLY
	// failed — its declared cause is payment_failed.
	if len(starter.causes) != 1 || starter.causes[0] != domain.DunningCausePaymentFailed {
		t.Fatalf("causes = %v, want [payment_failed]", starter.causes)
	}
}

// TestEnrollFailedWithoutDunning_CoolOffExcludesFresh locks the cool-off: a
// just-updated failed invoice is skipped so the inline SettleFailed StartDunning
// wins the common case and the sweep stays a pure backstop.
func TestEnrollFailedWithoutDunning_CoolOffExcludesFresh(t *testing.T) {
	inv := &mockInvoices{invoices: []domain.Invoice{failedBackfillInvoice("inv_fresh", time.Now().UTC())}}
	engine := noPMEngine(t, inv)
	starter := &recordingDunningStarter{}
	engine.SetDunningStarter(starter)

	swept, errsOut := engine.EnrollFailedWithoutDunning(context.Background(), 10)
	if swept != 0 || len(starter.started) != 0 || len(errsOut) != 0 {
		t.Fatalf("a fresh failed invoice must be inside the cool-off: swept=%d started=%v errs=%v", swept, starter.started, errsOut)
	}
}

// TestEnrollFailedWithoutDunning_CollectsPerInvoiceErrors: one bad row doesn't abort
// the sweep — the error is collected, not panicked.
func TestEnrollFailedWithoutDunning_CollectsPerInvoiceErrors(t *testing.T) {
	aged := time.Now().UTC().Add(-time.Hour)
	inv := &mockInvoices{invoices: []domain.Invoice{failedBackfillInvoice("inv_1", aged)}}
	engine := noPMEngine(t, inv)
	engine.SetDunningStarter(&recordingDunningStarter{err: errors.New("create run failed")})

	swept, errsOut := engine.EnrollFailedWithoutDunning(context.Background(), 10)
	if swept != 0 {
		t.Fatalf("swept = %d, want 0 (the only candidate errored)", swept)
	}
	if len(errsOut) != 1 {
		t.Fatalf("errors = %v, want 1", errsOut)
	}
}

// The sweep's tick binds no instant, so each invoice is enrolled on its own:
// one read of the engine's clock per invoice, when the sweep reaches it.
func TestEnrollFailedWithoutDunning_EachInvoiceOnItsOwnInstant(t *testing.T) {
	aged := time.Now().UTC().Add(-time.Hour)
	inv := &mockInvoices{invoices: []domain.Invoice{failedBackfillInvoice("inv_1", aged), failedBackfillInvoice("inv_2", aged)}}
	base := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)
	server := &steppingClock{at: base}
	engine := wireBaseTax(NewEngine(
		&mockSubs{cycleUpdated: make(map[string]bool)},
		&mockUsage{}, &mockPricing{}, inv, nil, &mockSettings{},
		&fakePaymentSetups{}, &recordingCharger{}, server,
	))
	starter := &recordingDunningStarter{}
	engine.SetDunningStarter(starter)

	if _, errs := engine.EnrollFailedWithoutDunning(context.Background(), 10); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	want := []time.Time{base.Add(time.Second), base.Add(2 * time.Second)}
	if server.reads != 2 || !slices.EqualFunc(starter.at, want, time.Time.Equal) {
		t.Errorf("engine clock read %d times, enrolled at %v; want one read per invoice, %v", server.reads, starter.at, want)
	}
}

// steppingClock moves a second each time it answers a read itself (no
// instant bound on ctx), and counts those reads.
type steppingClock struct {
	at    time.Time
	reads int
}

func (c *steppingClock) Now(ctx context.Context) time.Time {
	if t, ok := clock.EffectiveNow(ctx); ok {
		return t
	}
	c.reads++
	c.at = c.at.Add(time.Second)
	return c.at
}
