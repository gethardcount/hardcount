package invoice_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// execBypass runs one statement outside row-level security, for test setup.
func execBypass(t *testing.T, ctx context.Context, db *postgres.DB, sql string, args ...any) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// pendingCommit seeds a paid Stripe Tax invoice whose tax was never recorded
// at Stripe, last written lastWrite ago. expiresIn, when not zero, records its
// calculation with that expiry in the stored response, as Stripe Tax returns
// it; zero records no calculation at all.
func pendingCommit(t *testing.T, ctx context.Context, db *postgres.DB, name, calc string, lastWrite, expiresIn time.Duration) string {
	t.Helper()
	id := seedSweepInvoice(t, ctx, db, name, `
		UPDATE invoices SET status = 'paid', tax_provider = 'stripe_tax',
		       tax_status = 'ok', tax_calculation_id = '`+calc+`',
		       tax_transaction_id = '', is_simulated = false
		 WHERE id = $1`)
	execBypass(t, ctx, db, `UPDATE invoices SET updated_at = now() - make_interval(secs => $2) WHERE id = $1`, id, lastWrite.Seconds())
	if expiresIn != 0 {
		execBypass(t, ctx, db, `
			INSERT INTO tax_calculations (tenant_id, invoice_id, provider, provider_ref, request, response, created_at)
			SELECT tenant_id, id, 'stripe_tax', $2, '{}'::jsonb,
			       jsonb_build_object('id', $2::text, 'expires_at', extract(epoch from now() + make_interval(secs => $3))::bigint),
			       now() - make_interval(secs => $4)
			  FROM invoices WHERE id = $1`, id, calc, expiresIn.Seconds(), lastWrite.Seconds())
	}
	return id
}

func pendingCommitIDs(t *testing.T, ctx context.Context, store *invoice.PostgresStore) []string {
	t.Helper()
	pending, err := store.ListPendingTaxCommit(ctx, 500, false)
	if err != nil {
		t.Fatalf("ListPendingTaxCommit: %v", err)
	}
	ids := make([]string, 0, len(pending))
	for _, inv := range pending {
		ids = append(ids, inv.ID)
	}
	return ids
}

func position(ids []string, id string) int {
	for i, got := range ids {
		if got == id {
			return i
		}
	}
	return -1
}

// A tax commit is owed for as long as Stripe will take it: until the
// calculation's own expiry, 90 days after it was made. The list dropped an
// invoice 24 hours after its last write, from a mistaken reading of Stripe's
// limit. A tenant whose key could not record tax for three days had every
// invoice of the first two days left unreported for good, although Stripe
// would have accepted each of them once the key was fixed.
func TestListPendingTaxCommit_BoundedByTheCalculationsExpiry(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	store := invoice.NewPostgresStore(db)
	day := 24 * time.Hour

	threeDays := pendingCommit(t, ctx, db, "TaxCommit Three Days", "calc_3d", 3*day, 87*day)
	expired := pendingCommit(t, ctx, db, "TaxCommit Expired", "calc_expired", 91*day, -day)
	noRecordFresh := pendingCommit(t, ctx, db, "TaxCommit No Record Fresh", "calc_norecord_fresh", time.Hour, 0)
	noRecordOld := pendingCommit(t, ctx, db, "TaxCommit No Record Old", "calc_norecord_old", 3*day, 0)

	ids := pendingCommitIDs(t, ctx, store)
	if position(ids, threeDays) < 0 {
		t.Error("an invoice whose commit has failed for three days must stay listed: its calculation is good for 87 more days")
	}
	if position(ids, expired) >= 0 {
		t.Error("an invoice whose calculation has expired must not be listed: the commit can never succeed")
	}
	if position(ids, noRecordFresh) < 0 {
		t.Error("an invoice with no recorded calculation stays listed for 24 hours after its last write")
	}
	if position(ids, noRecordOld) >= 0 {
		t.Error("an invoice with no recorded calculation leaves the list 24 hours after its last write")
	}
}

// A failed commit puts its invoice off (DeferTaxWork). Until then the list
// skips it; after, it comes behind invoices that never failed. The list is
// across tenants and takes a fixed number per run, so without this one
// tenant's failing invoices held its head. The write that records the
// transaction ends the wait.
func TestListPendingTaxCommit_AFailedCommitGivesWay(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	store := invoice.NewPostgresStore(db)
	day := 24 * time.Hour

	failing := pendingCommit(t, ctx, db, "TaxCommit Failing", "calc_failing", 5*day, 85*day)
	healthy := pendingCommit(t, ctx, db, "TaxCommit Healthy", "calc_healthy", time.Hour, 89*day)
	tenantOf := func(id string) string {
		var tenantID string
		tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if err := tx.QueryRowContext(ctx, `SELECT tenant_id FROM invoices WHERE id = $1`, id).Scan(&tenantID); err != nil {
			t.Fatalf("read tenant: %v", err)
		}
		return tenantID
	}
	updatedAt := func(id string) time.Time {
		inv, err := store.Get(ctx, tenantOf(id), id)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return inv.UpdatedAt
	}

	// Older, so first: the head of the list.
	if ids := pendingCommitIDs(t, ctx, store); position(ids, failing) < 0 || position(ids, failing) > position(ids, healthy) {
		t.Fatalf("setup: want the older failing invoice listed ahead of the healthy one, got %v", ids)
	}

	before := updatedAt(failing)
	if err := store.DeferTaxWork(ctx, tenantOf(failing), failing, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("DeferTaxWork: %v", err)
	}
	if got := updatedAt(failing); !got.Equal(before) {
		t.Errorf("DeferTaxWork moved updated_at from %v to %v; the wait is measured from the last real write", before, got)
	}
	if ids := pendingCommitIDs(t, ctx, store); position(ids, failing) >= 0 || position(ids, healthy) < 0 {
		t.Fatalf("a deferred invoice must be skipped and the healthy one listed, got %v", ids)
	}

	// Its wait is over: listed again, behind the invoice that never failed.
	if err := store.DeferTaxWork(ctx, tenantOf(failing), failing, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("DeferTaxWork: %v", err)
	}
	if ids := pendingCommitIDs(t, ctx, store); position(ids, failing) < 0 || position(ids, failing) < position(ids, healthy) {
		t.Fatalf("after its wait a deferred invoice is listed behind one never deferred, got %v", ids)
	}

	if err := store.SetTaxTransaction(ctx, tenantOf(failing), failing, "tx_recorded"); err != nil {
		t.Fatalf("SetTaxTransaction: %v", err)
	}
	inv, err := store.Get(ctx, tenantOf(failing), failing)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if inv.TaxNextRetryAt != nil {
		t.Errorf("tax_next_retry_at = %v after the transaction was recorded, want it cleared", inv.TaxNextRetryAt)
	}
}

// The voided-invoice reversal list gives way the same way, and the write that
// stamps the reversal ends the wait.
func TestListPendingTaxReversal_AFailedReversalGivesWay(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	store := invoice.NewPostgresStore(db)

	seed := func(name string, lastWrite time.Duration) (id, tenantID string) {
		id = seedSweepInvoice(t, ctx, db, name, `
			UPDATE invoices SET status = 'voided', tax_provider = 'stripe_tax',
			       tax_status = 'ok', tax_transaction_id = 'tx_`+name[len(name)-1:]+`',
			       tax_reversed_at = NULL, is_simulated = false
			 WHERE id = $1`)
		execBypass(t, ctx, db, `UPDATE invoices SET updated_at = now() - make_interval(secs => $2) WHERE id = $1`, id, lastWrite.Seconds())
		tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if err := tx.QueryRowContext(ctx, `SELECT tenant_id FROM invoices WHERE id = $1`, id).Scan(&tenantID); err != nil {
			t.Fatalf("read tenant: %v", err)
		}
		return id, tenantID
	}
	listed := func() []string {
		pending, err := store.ListPendingTaxReversal(ctx, 500, false)
		if err != nil {
			t.Fatalf("ListPendingTaxReversal: %v", err)
		}
		ids := make([]string, 0, len(pending))
		for _, inv := range pending {
			ids = append(ids, inv.ID)
		}
		return ids
	}

	failing, failingTenant := seed("TaxReversal Failing A", 48*time.Hour)
	healthy, _ := seed("TaxReversal Healthy B", time.Hour)

	if err := store.DeferTaxWork(ctx, failingTenant, failing, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("DeferTaxWork: %v", err)
	}
	if ids := listed(); position(ids, failing) >= 0 || position(ids, healthy) < 0 {
		t.Fatalf("a deferred reversal must be skipped and the other listed, got %v", ids)
	}
	if err := store.DeferTaxWork(ctx, failingTenant, failing, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("DeferTaxWork: %v", err)
	}
	if ids := listed(); position(ids, failing) < 0 || position(ids, failing) < position(ids, healthy) {
		t.Fatalf("after its wait a deferred reversal is listed behind one never deferred, got %v", ids)
	}

	if err := store.MarkTaxReversed(ctx, failingTenant, failing); err != nil {
		t.Fatalf("MarkTaxReversed: %v", err)
	}
	inv, err := store.Get(ctx, failingTenant, failing)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if inv.TaxReversedAt == nil || inv.TaxNextRetryAt != nil {
		t.Errorf("after the reversal is stamped: reversed_at=%v next_retry=%v, want stamped and the wait cleared", inv.TaxReversedAt, inv.TaxNextRetryAt)
	}
}
