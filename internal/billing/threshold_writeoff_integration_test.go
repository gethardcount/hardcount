package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/subscription"
)

// firedAndWrittenOff fires one reset=false threshold invoice on the fixture
// (1000 calls at 1c against a 500c cap) and writes it off through the
// invoice service, as the operator's mark-uncollectible does.
func firedAndWrittenOff(t *testing.T, f *thresholdFixture, ctx context.Context) (*invoice.Service, domain.Invoice) {
	t.Helper()
	f.ingestUsage(t, ctx, 100, 10)
	resetFalse := false
	if _, err := f.subSvc.SetBillingThresholds(ctx, f.tenantID, f.subID, subscription.BillingThresholdsInput{
		AmountGTE: 500, ResetBillingCycle: &resetFalse,
	}); err != nil {
		t.Fatalf("set threshold: %v", err)
	}
	if fired, errs := f.engine.ScanThresholds(ctx, 50); len(errs) > 0 || fired != 1 {
		t.Fatalf("first scan: fired=%d errs=%v, want 1 fire", fired, errs)
	}
	invs := f.listInvoices(t, ctx)
	if len(invs) != 1 || invs[0].BillingReason != domain.BillingReasonThreshold {
		t.Fatalf("after the fire: %d invoices, want the one threshold invoice", len(invs))
	}
	invSvc := invoice.NewService(f.invStore, clock.Real(), f.settings)
	thr, err := invSvc.MarkUncollectible(ctx, f.tenantID, invs[0].ID)
	if err != nil || thr.Status != domain.InvoiceUncollectible {
		t.Fatalf("write off the threshold invoice: status=%s err=%v", thr.Status, err)
	}
	return invSvc, thr
}

// TestThresholdScan_FireOnceProbe_SeesWrittenOffAndVoided: the fire-once
// probe must find the cycle's threshold invoice in any status. The partial
// unique index (idx_invoices_threshold_unique_per_cycle) keeps one threshold
// invoice per (subscription, period start) whatever its status, so once a
// written-off or voided invoice holds that slot, no later fire this cycle can
// insert. Before the fix the probe skipped both statuses: every tick then
// re-evaluated the crossing, ran ApplyTaxToLineItems (a paid Stripe Tax
// calculation) and attempted the insert, which the index refused — the burn
// the probe exists to stop (ADR-065 §2).
//
// Mutation checks: probe with the watermark read (non-voided only) → the
// voided arm attempts an insert; probe with the pre-fix predicate (neither
// written-off nor voided) → both arms do.
func TestThresholdScan_FireOnceProbe_SeesWrittenOffAndVoided(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Threshold Probe Write-off")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()
	invSvc, thr := firedAndWrittenOff(t, f, ctx)

	tick := func(state string) {
		t.Helper()
		before := f.invAdapter.createTxCalls.Load()
		fired, errs := f.engine.ScanThresholds(ctx, 50)
		if len(errs) > 0 || fired != 0 {
			t.Fatalf("scan after the %s: fired=%d errs=%v, want nothing", state, fired, errs)
		}
		if n := f.invAdapter.createTxCalls.Load() - before; n != 0 {
			t.Errorf("scan after the %s attempted %d threshold invoice insert(s), want 0: the probe must see the %s invoice", state, n, state)
		}
	}
	tick("written-off")

	if _, err := invSvc.Void(ctx, f.tenantID, thr.ID); err != nil {
		t.Fatalf("void the written-off invoice: %v", err)
	}
	tick("voided")

	if n := len(f.listInvoices(t, ctx)); n != 1 {
		t.Errorf("%d invoices after the ticks, want 1", n)
	}
}

// TestThresholdBilledThroughTx_WriteOffKeepsVoidDrops: the plan swap's
// re-anchor guard (ADR-115) reads the threshold watermark on the swap's tx
// and refuses to re-anchor the period below it. A written-off threshold
// invoice still billed its window, so the guard must still see it; a voided
// one billed nothing, so it must not.
//
// Mutation check: put 'uncollectible' back in the watermark predicate → the
// written-off arm reads nil.
func TestThresholdBilledThroughTx_WriteOffKeepsVoidDrops(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Threshold Swap Guard Write-off")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()
	invSvc, thr := firedAndWrittenOff(t, f, ctx)

	billedThrough := func() *time.Time {
		t.Helper()
		sub, err := f.subStore.Get(ctx, f.tenantID, f.subID)
		if err != nil {
			t.Fatalf("get subscription: %v", err)
		}
		tx, err := f.db.BeginTx(ctx, postgres.TxTenant, f.tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		bt, err := f.engine.ThresholdBilledThroughTx(ctx, tx, sub)
		if err != nil {
			t.Fatalf("threshold billed-through: %v", err)
		}
		return bt
	}

	if bt := billedThrough(); bt == nil || !bt.Equal(thr.BillingPeriodEnd) {
		t.Errorf("billed-through after the write-off = %v, want the fire's end %v", bt, thr.BillingPeriodEnd)
	}
	if _, err := invSvc.Void(ctx, f.tenantID, thr.ID); err != nil {
		t.Fatalf("void the written-off invoice: %v", err)
	}
	if bt := billedThrough(); bt != nil {
		t.Errorf("billed-through after the void = %v, want nil: a voided invoice billed nothing", bt)
	}
}
