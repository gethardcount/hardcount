package billing_test

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/testutil"
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

// TestThresholdWriteOff_vs_Close_WriteOffInFlight is the concurrent-resolver
// schedule (playbook §5.3) for a write-off: the close has built its lines
// against the fire's watermark (residual [t1, P1) = 700c), and the operator
// writes the threshold invoice off before the close's tx opens. A write-off
// keeps the window billed, so the watermark the close re-reads on its tx is
// unchanged and the close commits the residual. Before the fix the re-read
// saw no watermark, the close re-planned, and it billed [P0, P1) = 1000c on
// top of the threshold invoice's 300c.
//
// Mutation check: put 'uncollectible' back in the watermark predicate → the
// cycle invoice's usage line starts at P0 and bills 1000c.
func TestThresholdWriteOff_vs_Close_WriteOffInFlight(t *testing.T) {
	r := newS1Rig(t, "Write-off vs close", 200, false)
	if fired := r.runFire(t); fired != 1 {
		t.Fatalf("fire reported %d, want 1", fired)
	}
	_, threshold, _ := r.liveInvoices(t)
	if len(threshold) != 1 {
		t.Fatalf("%d threshold invoices after the fire, want 1", len(threshold))
	}
	invSvc := invoice.NewService(r.f.invStore, clock.Real(), r.f.settings)
	r.closer.subs.beforeTx = func() {
		r.closer.subs.beforeTx = nil
		if _, err := invSvc.MarkUncollectible(r.at(r.t1.Add(time.Hour)), r.f.tenantID, threshold[0].ID); err != nil {
			t.Fatalf("write off in flight: %v", err)
		}
	}
	if gen := r.runClose(t); gen != 1 {
		t.Fatalf("close generated %d, want 1 (the residual)", gen)
	}
	cycle, _, _ := r.liveInvoices(t)
	if len(cycle) != 1 {
		t.Fatalf("%d cycle invoices, want 1", len(cycle))
	}
	lines := r.usageLines(t, cycle[0].ID)
	if len(lines) != 1 || lines[0].BillingPeriodStart == nil || !lines[0].BillingPeriodStart.Equal(r.t1) || lines[0].AmountCents != 700 {
		t.Fatalf("cycle usage lines = %+v, want one residual line [t1, P1) = 700c", lines)
	}
	assertUsageBilledOnce(t, r.base)
}

// TestThresholdVoid_vs_Close_VoidInFlight is the same schedule with a void,
// the control: a void gives the window back, so the close's re-read sees the
// watermark gone, re-plans and bills [P0, P1) = 1000c.
func TestThresholdVoid_vs_Close_VoidInFlight(t *testing.T) {
	r := newS1Rig(t, "Void vs close", 200, false)
	if fired := r.runFire(t); fired != 1 {
		t.Fatalf("fire reported %d, want 1", fired)
	}
	_, threshold, _ := r.liveInvoices(t)
	if len(threshold) != 1 {
		t.Fatalf("%d threshold invoices after the fire, want 1", len(threshold))
	}
	invSvc := invoice.NewService(r.f.invStore, clock.Real(), r.f.settings)
	r.closer.subs.beforeTx = func() {
		r.closer.subs.beforeTx = nil
		if _, err := invSvc.Void(r.at(r.t1.Add(time.Hour)), r.f.tenantID, threshold[0].ID); err != nil {
			t.Fatalf("void in flight: %v", err)
		}
	}
	if gen := r.runClose(t); gen != 1 {
		t.Fatalf("close generated %d, want 1", gen)
	}
	cycle, live, _ := r.liveInvoices(t)
	if len(cycle) != 1 || len(live) != 0 {
		t.Fatalf("invoices: cycle=%d live threshold=%d, want 1 and 0", len(cycle), len(live))
	}
	var billed int64
	for _, l := range r.usageLines(t, cycle[0].ID) {
		billed += l.AmountCents
		if l.BillingPeriodStart == nil || !l.BillingPeriodStart.Equal(r.f.cycleStart) {
			t.Errorf("cycle usage line starts %v, want P0 %v: the void gave the window back", l.BillingPeriodStart, r.f.cycleStart)
		}
	}
	if billed != 1000 {
		t.Fatalf("cycle usage = %dc, want 1000c", billed)
	}
	assertUsageBilledOnce(t, r.base)
}

// ---------------------------------------------------------------------------
// A void racing a closer: the void must land wholly before the closer's
// commit (the closer bills the window) or wholly after it (the window is
// forgiven). Never in between.
// ---------------------------------------------------------------------------

// racingVoid is a void of the threshold invoice run on its own goroutine.
// When it returns it counts the subscription's invoices other than the
// threshold one: 0 means the closer had not committed yet, so the void came
// first and the closer must bill the window.
type racingVoid struct {
	done      chan struct{}
	err       error
	committed int // the closer's invoices visible when the void returned
}

func startVoid(r *s1Rig, invSvc *invoice.Service, admin *sql.DB, thrID string, at time.Time) *racingVoid {
	return startRacing(r, admin, func() error {
		_, err := invSvc.Void(r.at(at), r.f.tenantID, thrID)
		return err
	})
}

// startRacing runs action (a status change of the threshold invoice) on its
// own goroutine, as startVoid does.
func startRacing(r *s1Rig, admin *sql.DB, action func() error) *racingVoid {
	v := &racingVoid{done: make(chan struct{})}
	go func() {
		defer close(v.done)
		if v.err = action(); v.err != nil {
			return
		}
		v.err = admin.QueryRowContext(r.base, `
			SELECT count(*) FROM invoices
			WHERE subscription_id = $1 AND billing_reason <> 'threshold'`, r.f.subID).Scan(&v.committed)
	}()
	return v
}

// lockWaiters counts this database's backends blocked on a lock. The tests
// below run one closer and one void, so a waiter is whichever of the two is
// not holding the row. (The statements read every invoice column, longer
// than pg_stat_activity keeps of a query, so the text cannot tell them
// apart.)
func lockWaiters(ctx context.Context, admin *sql.DB) (int, error) {
	var n int
	err := admin.QueryRowContext(ctx, `
		SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n)
	return n, err
}

// settle returns once the void has returned (true) or is blocked on the
// invoice row lock (false). It runs inside the closer's hook, on the test
// goroutine.
func (v *racingVoid) settle(t *testing.T, ctx context.Context, admin *sql.DB) (returned bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		select {
		case <-v.done:
			return true
		default:
		}
		n, err := lockWaiters(ctx, admin)
		if err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if n > 0 {
			return false
		}
		if time.Now().After(deadline) {
			t.Fatalf("the void neither returned nor waited on the invoice row within 15s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// wantOrderHonored checks the closer's usage against the order the void and
// the closer committed in: void first, the closer bills from P0 (fullCents);
// closer first, it bills only from the fire (residualCents) and the window is
// forgiven.
func (v *racingVoid) wantOrderHonored(t *testing.T, r *s1Rig, closerInv domain.Invoice, fullCents, residualCents int64) {
	t.Helper()
	<-v.done
	if v.err != nil {
		t.Fatalf("void: %v", v.err)
	}
	var billed int64
	var from *time.Time
	for _, l := range r.usageLines(t, closerInv.ID) {
		billed += l.AmountCents
		if from == nil || (l.BillingPeriodStart != nil && l.BillingPeriodStart.Before(*from)) {
			from = l.BillingPeriodStart
		}
	}
	if v.committed == 0 {
		if billed != fullCents || from == nil || !from.Equal(r.f.cycleStart) {
			t.Fatalf("the void returned before the %s invoice was committed, so that invoice must bill the window from P0 (%dc); it billed %dc from %v: the window is billed nowhere though the void came first",
				closerInv.BillingReason, fullCents, billed, from)
		}
		return
	}
	if billed != residualCents || from == nil || !from.Equal(r.t1) {
		t.Fatalf("the void returned after the %s invoice was committed, so that invoice bills only from the fire (%dc from t1); it billed %dc from %v",
			closerInv.BillingReason, residualCents, billed, from)
	}
}

// TestThresholdVoid_vs_Close_VoidAfterReRead: the close re-read the
// watermark on its tx (the fire's invoice, finalized), and then the operator
// voids that invoice before the close commits. The close's re-read holds the
// row FOR KEY SHARE, so the void (FOR UPDATE) waits for the close's commit
// and lands after it: the window is forgiven, as for any void after the period's invoice.
// Before, the void committed first and the close then committed the
// residual it had planned: a void that came first, and the window billed
// nowhere.
func TestThresholdVoid_vs_Close_VoidAfterReRead(t *testing.T) {
	r := newS1Rig(t, "Void after the close's re-read", 200, false)
	if fired := r.runFire(t); fired != 1 {
		t.Fatalf("fire reported %d, want 1", fired)
	}
	_, threshold, _ := r.liveInvoices(t)
	if len(threshold) != 1 {
		t.Fatalf("%d threshold invoices after the fire, want 1", len(threshold))
	}
	admin := testutil.AdminPool(t)
	invSvc := invoice.NewService(r.f.invStore, clock.Real(), r.f.settings)
	var v *racingVoid
	r.closer.invs.afterWatermarkTx = func() {
		r.closer.invs.afterWatermarkTx = nil
		v = startVoid(r, invSvc, admin, threshold[0].ID, r.t1.Add(time.Hour))
		v.settle(t, r.base, admin)
	}
	if gen := r.runClose(t); gen != 1 {
		t.Fatalf("close generated %d, want 1", gen)
	}
	if v == nil {
		t.Fatal("the close never re-read the watermark on its tx")
	}
	cycle, _, _ := r.liveInvoices(t)
	if len(cycle) != 1 {
		t.Fatalf("%d cycle invoices, want 1", len(cycle))
	}
	v.wantOrderHonored(t, r, cycle[0], 1000, 700)
	assertUsageBilledOnce(t, r.base)
}

// TestThresholdPayment_vs_Close_PaymentAfterReRead is the paid-flip control
// for the row lock: an offline payment of the threshold invoice after the
// close's re-read does not wait for the close (the payment's FOR NO KEY
// UPDATE against the re-read's FOR KEY SHARE), and the close bills the
// residual whichever lands first: a paid invoice keeps its window billed.
func TestThresholdPayment_vs_Close_PaymentAfterReRead(t *testing.T) {
	r := newS1Rig(t, "Payment after the close's re-read", 200, false)
	if fired := r.runFire(t); fired != 1 {
		t.Fatalf("fire reported %d, want 1", fired)
	}
	_, threshold, _ := r.liveInvoices(t)
	if len(threshold) != 1 {
		t.Fatalf("%d threshold invoices after the fire, want 1", len(threshold))
	}
	admin := testutil.AdminPool(t)
	invSvc := invoice.NewService(r.f.invStore, clock.Real(), r.f.settings)
	var p *racingVoid
	r.closer.invs.afterWatermarkTx = func() {
		r.closer.invs.afterWatermarkTx = nil
		p = startRacing(r, admin, func() error {
			_, err := invSvc.RecordOfflinePayment(r.at(r.t1.Add(time.Hour)), r.f.tenantID, threshold[0].ID, "wire")
			return err
		})
		p.settle(t, r.base, admin)
	}
	if gen := r.runClose(t); gen != 1 {
		t.Fatalf("close generated %d, want 1", gen)
	}
	if p == nil {
		t.Fatal("the close never re-read the watermark on its tx")
	}
	<-p.done
	if p.err != nil {
		t.Fatalf("offline payment: %v", p.err)
	}
	cycle, threshold, _ := r.liveInvoices(t)
	if len(cycle) != 1 || len(threshold) != 1 || threshold[0].Status != domain.InvoicePaid {
		t.Fatalf("invoices: cycle=%d threshold=%+v, want one cycle invoice and the threshold invoice paid", len(cycle), threshold)
	}
	lines := r.usageLines(t, cycle[0].ID)
	if len(lines) != 1 || lines[0].BillingPeriodStart == nil || !lines[0].BillingPeriodStart.Equal(r.t1) || lines[0].AmountCents != 700 {
		t.Fatalf("cycle usage lines = %+v, want one residual line [t1, P1) = 700c", lines)
	}
	assertUsageBilledOnce(t, r.base)
}

// TestThresholdPayment_vs_Close_DunningPauseInFlight: dunning's pause for
// the threshold invoice T (SetDunningPause, held open here after its three
// statements) holds the subscription and T when a payment of T starts. The
// payment's LockDunningPauseTx reads before the pause commits, so it does
// not lock the subscription, and it then waits on T. The close then waits
// on the subscription. When the pause commits, the payment holds T and the
// close holds the subscription; the payment's pause release then waits for
// the close. The close's watermark read must not wait for the payment, or
// the two deadlock and Postgres fails one of them (40P01).
func TestThresholdPayment_vs_Close_DunningPauseInFlight(t *testing.T) {
	r := newS1Rig(t, "Payment vs close, dunning pause in flight", 200, false)
	r.f.invStore.SetDunningPauseReleaser(r.f.subStore) // as the server wires it
	if fired := r.runFire(t); fired != 1 {
		t.Fatalf("fire reported %d, want 1", fired)
	}
	_, threshold, _ := r.liveInvoices(t)
	if len(threshold) != 1 {
		t.Fatalf("%d threshold invoices after the fire, want 1", len(threshold))
	}
	thrID := threshold[0].ID
	admin := testutil.AdminPool(t)
	waitFor := func(n int) {
		t.Helper()
		for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			got, err := lockWaiters(r.base, admin)
			if err != nil {
				t.Errorf("read pg_stat_activity: %v", err)
				return
			}
			if got >= n {
				return
			}
		}
		t.Errorf("lock waiters never reached %d", n)
	}

	paidAt := r.t1.Add(time.Hour)
	payDone := make(chan error, 1)
	pauseDone := make(chan error, 1)
	r.closer.subs.beforeTx = func() {
		r.closer.subs.beforeTx = nil
		pause, err := r.f.db.BeginTx(r.base, postgres.TxTenant, r.f.tenantID)
		if err != nil {
			t.Fatalf("begin pause tx: %v", err)
		}
		// SetDunningPause's statements, in its order.
		var st string
		if err := pause.QueryRowContext(r.base, `SELECT status FROM subscriptions WHERE id = $1 FOR NO KEY UPDATE`, r.f.subID).Scan(&st); err != nil {
			t.Fatalf("pause: lock subscription: %v", err)
		}
		if err := pause.QueryRowContext(r.base, `SELECT status FROM invoices WHERE id = $1 FOR NO KEY UPDATE`, thrID).Scan(&st); err != nil {
			t.Fatalf("pause: lock invoice: %v", err)
		}
		if _, err := pause.ExecContext(r.base, `
			UPDATE subscriptions SET pause_collection_behavior = 'keep_as_draft',
				pause_collection_resumes_at = NULL, pause_collection_invoice_id = $1, updated_at = now()
			WHERE id = $2`, thrID, r.f.subID); err != nil {
			t.Fatalf("pause: write: %v", err)
		}
		go func() {
			_, err := r.f.invStore.MarkPaid(r.at(paidAt), r.f.tenantID, thrID, "", paidAt)
			payDone <- err
		}()
		waitFor(1) // the payment waits on T
		go func() {
			waitFor(2) // and the close on the subscription
			pauseDone <- pause.Commit()
		}()
	}
	gen, failures := r.closer.engine.RunCycleForTenant(r.at(r.close), r.f.tenantID, 50)
	if err := <-pauseDone; err != nil {
		t.Fatalf("pause commit: %v", err)
	}
	payErr := <-payDone
	if payErr != nil || len(failures) != 0 {
		t.Fatalf("payment err = %v, close failures = %v; want both to succeed", payErr, failures)
	}
	if gen != 1 {
		t.Fatalf("close generated %d, want 1", gen)
	}
	cycle, threshold, _ := r.liveInvoices(t)
	if len(cycle) != 1 || len(threshold) != 1 || threshold[0].Status != domain.InvoicePaid {
		t.Fatalf("invoices: cycle=%d threshold=%+v, want one cycle invoice and the threshold invoice paid", len(cycle), threshold)
	}
	lines := r.usageLines(t, cycle[0].ID)
	if len(lines) != 1 || lines[0].BillingPeriodStart == nil || !lines[0].BillingPeriodStart.Equal(r.t1) || lines[0].AmountCents != 700 {
		t.Fatalf("cycle usage lines = %+v, want one residual line [t1, P1) = 700c", lines)
	}
	if sub := r.sub(t); sub.PauseCollection == nil || sub.PauseCollection.ResumesAt == nil {
		t.Fatalf("pause = %+v, want the payment to have lifted it (resumes_at set)", sub.PauseCollection)
	}
	assertUsageBilledOnce(t, r.base)
}

// blockingReverser stands in for the credit reverser so a void can be held
// inside its transaction, after it has locked the invoice row: entered is
// closed when the void gets there, and it returns once release is closed.
type blockingReverser struct {
	entered, release chan struct{}
}

func (b *blockingReverser) ReverseForInvoiceTx(context.Context, *sql.Tx, string, string, string, string) (int64, error) {
	close(b.entered)
	<-b.release
	return 0, nil
}

func (b *blockingReverser) RetireCommitGrantForInvoiceTx(context.Context, *sql.Tx, string, string) (int64, error) {
	return 0, nil
}

// TestThresholdVoid_vs_Close_VoidHoldsTheRow is the other order: the void
// has locked the threshold invoice and is mid-transaction when the close
// re-reads the watermark. The re-read waits for the void, sees the invoice
// voided, and the close re-plans and bills the window from P0. The control
// for the lock: a re-read that returned the row as it was before the void
// would commit the residual after a void that committed first.
func TestThresholdVoid_vs_Close_VoidHoldsTheRow(t *testing.T) {
	r := newS1Rig(t, "Void holds the row at the close's re-read", 200, false)
	if fired := r.runFire(t); fired != 1 {
		t.Fatalf("fire reported %d, want 1", fired)
	}
	_, threshold, _ := r.liveInvoices(t)
	if len(threshold) != 1 {
		t.Fatalf("%d threshold invoices after the fire, want 1", len(threshold))
	}
	admin := testutil.AdminPool(t)
	invSvc := invoice.NewService(r.f.invStore, clock.Real(), r.f.settings)
	rev := &blockingReverser{entered: make(chan struct{}), release: make(chan struct{})}
	invSvc.SetCreditReverser(rev)
	var v *racingVoid
	r.closer.subs.beforeTx = func() {
		r.closer.subs.beforeTx = nil
		v = startVoid(r, invSvc, admin, threshold[0].ID, r.t1.Add(time.Hour))
		select {
		case <-rev.entered:
		case <-v.done:
			t.Fatalf("void returned before it reached its credit reversal: %v", v.err)
		case <-time.After(15 * time.Second):
			t.Fatal("void did not reach its credit reversal within 15s")
		}
		// Release the void once the close's re-read waits for it (or after
		// 3s, when nothing waits).
		go func() {
			defer close(rev.release)
			for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				if n, err := lockWaiters(r.base, admin); err == nil && n > 0 {
					return
				}
			}
		}()
	}
	if gen := r.runClose(t); gen != 1 {
		t.Fatalf("close generated %d, want 1", gen)
	}
	if v == nil {
		t.Fatal("the close's hook did not run")
	}
	cycle, _, _ := r.liveInvoices(t)
	if len(cycle) != 1 {
		t.Fatalf("%d cycle invoices, want 1", len(cycle))
	}
	v.wantOrderHonored(t, r, cycle[0], 1000, 700)
	assertUsageBilledOnce(t, r.base)
}

// TestThresholdVoid_vs_ImmediateCancel_VoidAfterRead: the immediate cancel
// is a closer of the same window. It reads the watermark after the cancel's
// UPDATE locked the subscription, on the cancel's tx, FOR KEY SHARE, so a void
// of the threshold invoice that arrives after the read waits for the cancel
// and is forgiven. Before, the read ran on its own connection: the void
// committed first and the cancel's final invoice billed only [t1, tc).
func TestThresholdVoid_vs_ImmediateCancel_VoidAfterRead(t *testing.T) {
	r := newS1Rig(t, "Void after the cancel's read", 200, false)
	if fired := r.runFire(t); fired != 1 {
		t.Fatalf("fire reported %d, want 1", fired)
	}
	_, threshold, _ := r.liveInvoices(t)
	if len(threshold) != 1 {
		t.Fatalf("%d threshold invoices after the fire, want 1", len(threshold))
	}
	admin := testutil.AdminPool(t)
	invSvc := invoice.NewService(r.f.invStore, clock.Real(), r.f.settings)
	var v *racingVoid
	var once sync.Once
	hook := func() {
		once.Do(func() {
			v = startVoid(r, invSvc, admin, threshold[0].ID, r.t1.Add(time.Minute))
			v.settle(t, r.base, admin)
		})
	}
	r.closer.invs.afterWatermark, r.closer.invs.afterWatermarkTx = hook, hook
	ctxCancel := r.at(r.tc)
	if _, err := r.f.subStore.CancelAtomicWithBill(ctxCancel, r.f.tenantID, r.f.subID, func(tx *sql.Tx, canceled domain.Subscription) error {
		_, _, err := r.closer.engine.BillFinalOnImmediateCancelTx(ctxCancel, tx, canceled)
		return err
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if v == nil {
		t.Fatal("the cancel never read the watermark")
	}
	_, _, other := r.liveInvoices(t)
	if len(other) != 1 || other[0].BillingReason != domain.BillingReasonSubscriptionCancel {
		t.Fatalf("invoices other than the threshold one: %d, want the one final cancel invoice", len(other))
	}
	v.wantOrderHonored(t, r, other[0], 400, 100)
	assertUsageBilledOnce(t, r.base)
}

// TestThresholdVoid_vs_PlanSwap_VoidAfterRead: the cross-interval swap
// closes [P0, swap) on its own invoice. Its tx reads the watermark twice
// (the re-anchor guard, then BillOnPlanSwapTx), and the first read takes
// the row FOR KEY SHARE, so a void after the second read waits for the swap and
// is forgiven. Before, the void committed first and the swap invoice billed
// only [t1, swap).
func TestThresholdVoid_vs_PlanSwap_VoidAfterRead(t *testing.T) {
	r := newS1Rig(t, "Void after the swap's read", 200, false)
	sw, _ := newInAdvanceSwapRig(t, r)
	if fired := r.runFire(t); fired != 1 {
		t.Fatalf("fire reported %d, want 1", fired)
	}
	_, threshold, _ := r.liveInvoices(t)
	if len(threshold) != 1 {
		t.Fatalf("%d threshold invoices after the fire, want 1", len(threshold))
	}
	admin := testutil.AdminPool(t)
	invSvc := invoice.NewService(r.f.invStore, clock.Real(), r.f.settings)
	swapAt := r.f.cycleStart.Add(3 * time.Hour)
	var v *racingVoid
	reads := 0
	r.closer.invs.afterWatermarkTx = func() {
		reads++
		if reads != 2 {
			return
		}
		v = startVoid(r, invSvc, admin, threshold[0].ID, r.t1.Add(time.Hour))
		v.settle(t, r.base, admin)
	}
	if err := sw.swap(t, r, swapAt); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if v == nil {
		t.Fatalf("the swap read the watermark %d time(s) on its tx, want 2", reads)
	}
	_, _, other := r.liveInvoices(t)
	var swapInv []domain.Invoice
	for _, inv := range other {
		if inv.BillingPeriodStart.Equal(swapAt) {
			swapInv = append(swapInv, inv)
		}
	}
	if len(swapInv) != 1 {
		t.Fatalf("%d swap invoices headed at %v, want 1", len(swapInv), swapAt)
	}
	v.wantOrderHonored(t, r, swapInv[0], 1000, 700)
	assertUsageBilledOnce(t, r.base)
}

// TestThresholdResetTrue_VoidOrWriteOff_CloseBillsOnlyTheNewPeriod is the
// reset_billing_cycle=true control. The fire bills [P0, t1) and starts a
// new period at t1, so the next close bills only [t1, end) whatever happens
// to the threshold invoice: a write-off keeps [P0, t1) billed on it, and a
// void, even before that close, does not give it back. The void dialog and
// the API describe both settings, so this is what they must match.
func TestThresholdResetTrue_VoidOrWriteOff_CloseBillsOnlyTheNewPeriod(t *testing.T) {
	for _, action := range []string{"void", "write-off"} {
		t.Run(action, func(t *testing.T) {
			r := newS1Rig(t, "Reset true "+action, 200, true)
			if fired := r.runFire(t); fired != 1 {
				t.Fatalf("fire reported %d, want 1", fired)
			}
			_, threshold, _ := r.liveInvoices(t)
			if len(threshold) != 1 || threshold[0].SubtotalCents != 300 {
				t.Fatalf("threshold invoices after the fire: %+v, want one of 300c", threshold)
			}
			sub := r.assertPeriod(t, r.t1, nil)
			invSvc := invoice.NewService(r.f.invStore, clock.Real(), r.f.settings)
			ctx := r.at(r.t1.Add(time.Hour))
			var err error
			if action == "void" {
				_, err = invSvc.Void(ctx, r.f.tenantID, threshold[0].ID)
			} else {
				_, err = invSvc.MarkUncollectible(ctx, r.f.tenantID, threshold[0].ID)
			}
			if err != nil {
				t.Fatalf("%s the threshold invoice: %v", action, err)
			}

			gen, failures := r.closer.engine.RunCycleForTenant(r.at(sub.CurrentBillingPeriodEnd.Add(time.Nanosecond)), r.f.tenantID, 50)
			if len(failures) != 0 || gen != 1 {
				t.Fatalf("close of [t1, end): generated %d, failures %v, want 1 and none", gen, failures)
			}
			cycle, _, _ := r.liveInvoices(t)
			if len(cycle) != 1 || !cycle[0].BillingPeriodStart.Equal(r.t1) {
				t.Fatalf("cycle invoices: %d, want one headed at t1", len(cycle))
			}
			var billed int64
			for _, l := range r.usageLines(t, cycle[0].ID) {
				billed += l.AmountCents
				if l.BillingPeriodStart == nil || l.BillingPeriodStart.Before(r.t1) {
					t.Errorf("cycle usage line starts %v, before the fire t1 %v: the %s gave [P0, t1) back", l.BillingPeriodStart, r.t1, action)
				}
			}
			if billed != 700 {
				t.Fatalf("cycle usage = %dc, want 700c (only [t1, end))", billed)
			}
			assertUsageBilledOnce(t, r.base)
		})
	}
}
