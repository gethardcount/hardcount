package invoice_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/platform/walk"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// stillPendingRetrier records each draft it is asked to tax and leaves it
// waiting, as a provider that is still down does.
type stillPendingRetrier struct{ tried map[string]int }

func (r *stillPendingRetrier) RetryTaxForInvoice(_ context.Context, _, invoiceID string, _ func(context.Context, *sql.Tx, domain.Invoice) error) (domain.Invoice, error) {
	r.tried[invoiceID]++
	return domain.Invoice{ID: invoiceID, Status: domain.InvoiceDraft, TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxPending}}, nil
}

func (r *stillPendingRetrier) ComputeTaxForInvoice(context.Context, string, string) (domain.Invoice, error) {
	return domain.Invoice{}, nil
}

// seedClockTaxDrafts makes n drafts waiting for a tax retry, for one customer
// on a new test clock, and returns the clock.
func seedClockTaxDrafts(t *testing.T, db *postgres.DB, ctx context.Context, tenantID string, store *invoice.PostgresStore, n int) string {
	t.Helper()
	clockID, custID := postgres.NewID("hc_tclk"), postgres.NewID("hc_cus")
	now := time.Now().UTC()
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO test_clocks (id, tenant_id, name, frozen_time, status) VALUES ($1, $2, 'tax', $3, 'advancing')`, clockID, tenantID, now); err != nil {
		t.Fatalf("seed clock: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO customers (id, tenant_id, external_id, display_name, email, test_clock_id, created_at, updated_at)
		VALUES ($1, $2, 'tax-clock', 'Tax Clock', '', $3, $4, $4)`, custID, tenantID, clockID, now); err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	period := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	due := period.AddDate(0, 0, 30)
	for i := range n {
		if _, err := store.CreateWithLineItems(ctx, tenantID, domain.Invoice{
			CustomerID: custID, InvoiceNumber: fmt.Sprintf("HC-CLOCKTAX-%04d", i),
			Status: domain.InvoiceDraft, PaymentStatus: domain.PaymentPending, Currency: "USD",
			BillingPeriodStart: period, BillingPeriodEnd: period.AddDate(0, 1, 0),
			IssuedAt: &period, DueAt: &due, NetPaymentTermDays: 30,
		}, nil); err != nil {
			t.Fatalf("create invoice %d: %v", i, err)
		}
	}
	tx, err = db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE invoices SET tax_status = 'pending', tax_error_code = 'provider_outage', tax_retry_count = 0 WHERE customer_id = $1`, custID); err != nil {
		t.Fatalf("mark pending: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	return clockID
}

// A clock's tax-retry job tries every draft waiting for tax, once each: a
// clock has no next tick to pick up a second page, and a draft the try left
// waiting stays in the list, so the job must move past it. Two pages and one
// row more prove both.
func TestRetryPendingTaxForClock_TriesEveryDraftOnce(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Clock Tax Every Draft")
	store := invoice.NewPostgresStore(db)
	const n = 2*100 + 1

	clockID := seedClockTaxDrafts(t, db, ctx, tenantID, store, n)

	retrier := &stillPendingRetrier{tried: map[string]int{}}
	svc := invoice.NewService(store, nil, tenant.NewSettingsStore(db))
	svc.SetTaxRetrier(retrier)
	done := make(chan []error, 1)
	go func() {
		_, errs := svc.RetryPendingTaxForClock(ctx, tenantID, clockID, 100)
		done <- errs
	}()
	select {
	case errs := <-done:
		if len(errs) > 0 {
			t.Fatalf("errors: %v", errs[0])
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the job did not end: it read the waiting drafts again")
	}
	if len(retrier.tried) != n {
		t.Errorf("drafts tried = %d, want %d: the job stopped before the last page", len(retrier.tried), n)
	}
	for id, times := range retrier.tried {
		if times != 1 {
			t.Errorf("draft %s tried %d times, want once", id, times)
		}
	}
}

// refusingRetrier refuses every draft, as a provider error would.
type refusingRetrier struct{ tried map[string]int }

func (r *refusingRetrier) RetryTaxForInvoice(_ context.Context, _, invoiceID string, _ func(context.Context, *sql.Tx, domain.Invoice) error) (domain.Invoice, error) {
	r.tried[invoiceID]++
	return domain.Invoice{}, errors.New("tax provider refused")
}

func (r *refusingRetrier) ComputeTaxForInvoice(context.Context, string, string) (domain.Invoice, error) {
	return domain.Invoice{}, nil
}

// A draft whose retry fails stays in the list. The cursor moves past it, so
// each draft is tried once and the job ends with one error per draft.
func TestRetryPendingTaxForClock_DraftsThatStayListed(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Clock Tax Stay Listed")
	store := invoice.NewPostgresStore(db)
	const n = 2*100 + 1
	clockID := seedClockTaxDrafts(t, db, ctx, tenantID, store, n)

	retrier := &refusingRetrier{tried: map[string]int{}}
	svc := invoice.NewService(store, nil, tenant.NewSettingsStore(db))
	svc.SetTaxRetrier(retrier)
	runCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	done := make(chan []error, 1)
	go func() {
		_, errs := svc.RetryPendingTaxForClock(runCtx, tenantID, clockID, 100)
		done <- errs
	}()
	select {
	case errs := <-done:
		if len(errs) != n {
			t.Errorf("errors = %d, want %d: one per draft, each draft tried once", len(errs), n)
		}
	case <-runCtx.Done():
		t.Fatal("the job did not end: it read the refused drafts again")
	}
	if len(retrier.tried) != n {
		t.Errorf("drafts tried = %d, want %d", len(retrier.tried), n)
	}
}

// Walking the clock's tax-retry list page by page, as the job does, returns
// every waiting draft exactly once when the order the drafts were created in
// disagrees with the order they were inserted in (and so with id order) and
// with the order they were touched in: the last inserted were created first,
// and the first-created drafts were touched last. The page's order and the cursor must both be
// (created_at, id); a page ordered by updated_at would end on a late-created
// draft and leave the earlier ones behind the cursor, and a cursor on
// updated_at would meet drafts again.
func TestListPendingTaxRetryForClock_WalkReturnsEveryDraftOnce(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Clock Tax Walk")
	store := invoice.NewPostgresStore(db)
	const n, page = 5, 2
	clockID := seedClockTaxDrafts(t, db, ctx, tenantID, store, n)

	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(ctx, `SELECT id FROM invoices WHERE invoice_number LIKE 'HC-CLOCKTAX-%' ORDER BY invoice_number`)
	if err != nil {
		t.Fatalf("list drafts: %v", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	base := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	created := func(i int) int { return n - 1 - i } // the last inserted were created first
	for i, id := range ids {
		at := base.Add(time.Duration(created(i)) * time.Minute)
		updated := at
		if created(i) < 3 {
			updated = updated.Add(time.Hour) // touched last
		}
		if _, err := tx.ExecContext(ctx, `UPDATE invoices SET created_at = $2, updated_at = $3 WHERE id = $1`,
			id, at, updated); err != nil {
			t.Fatalf("set times: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	seen := map[string]int{}
	var after walk.Key
	for range n + 1 {
		got, err := store.ListPendingTaxRetryForClock(ctx, tenantID, clockID, domain.TaxRetryableErrorCodes(), domain.MaxTaxRetryAttempts, after, page)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, inv := range got {
			seen[inv.ID]++
		}
		if len(got) < page {
			break
		}
		after = walk.Key{At: got[len(got)-1].CreatedAt, ID: got[len(got)-1].ID}
	}
	if len(ids) != n {
		t.Fatalf("seeded %d drafts, want %d", len(ids), n)
	}
	for i, id := range ids {
		if seen[id] != 1 {
			t.Errorf("draft created %d of %d returned %d times, want once", created(i)+1, n, seen[id])
		}
	}
}
