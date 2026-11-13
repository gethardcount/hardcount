package creditnote_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// seedClockReliefDrafts makes n relief drafts held for issue on one paid
// invoice of a customer on a test clock, and returns the clock and the notes.
func seedClockReliefDrafts(t *testing.T, db *postgres.DB, ctx context.Context, tenantID string, n int) (string, []domain.CreditNote) {
	t.Helper()
	clockID := seedClockRow(t, db, tenantID)
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_relief_clock", DisplayName: "Relief Clock", TestClockID: clockID})
	if err != nil {
		t.Fatalf("customer: %v", err)
	}
	now := time.Now().UTC()
	inv, err := invoice.NewPostgresStore(db).CreateWithLineItems(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "RELIEF-CLOCK-INV", Status: domain.InvoicePaid,
		PaymentStatus: domain.PaymentSucceeded, Currency: "USD",
		SubtotalCents: 1_000_000, TotalAmountCents: 1_000_000, AmountPaidCents: 1_000_000,
		BillingPeriodStart: now.Add(-30 * 24 * time.Hour), BillingPeriodEnd: now, IssuedAt: &now,
	}, nil)
	if err != nil {
		t.Fatalf("invoice: %v", err)
	}
	store := creditnote.NewPostgresStore(db)
	notes := make([]domain.CreditNote, n)
	for i := range n {
		cn, err := store.Create(ctx, tenantID, domain.CreditNote{
			InvoiceID: inv.ID, CustomerID: cust.ID, CreditNoteNumber: fmt.Sprintf("CN-RELIEF-%04d", i),
			Status: domain.CreditNoteDraft, Reason: "subscription_cancellation",
			SubtotalCents: 100, TotalCents: 100, CreditAmountCents: 100,
			Currency: "USD", RefundStatus: domain.RefundNone, IssuePending: true,
		})
		if err != nil {
			t.Fatalf("draft %d: %v", i, err)
		}
		notes[i] = cn
	}
	execBypassCN(t, db, `UPDATE credit_notes SET is_simulated = true WHERE customer_id = $1`, cust.ID)
	return clockID, notes
}

// A clock's relief-note job issues every held relief note: a clock has no
// next tick to pick up a second page. Two pages and one note more prove it.
func TestRetryPendingClawbackIssueForClock_IssuesEveryDraft(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Clock Relief Every Draft")
	const n = 2*100 + 1
	clockID, notes := seedClockReliefDrafts(t, db, ctx, tenantID, n)

	svc := creditnote.NewService(creditnote.NewPostgresStore(db), invoice.NewPostgresStore(db), nil, okGranterTx{})
	issued, errs := svc.RetryPendingClawbackIssueForClock(ctx, tenantID, clockID, time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC), 100)
	if len(errs) > 0 {
		t.Fatalf("errors: %v", errs[0])
	}
	if issued != n {
		t.Errorf("issued %d, want %d: the job stopped before the last page", issued, n)
	}
	last, err := creditnote.NewPostgresStore(db).Get(ctx, tenantID, notes[n-1].ID)
	if err != nil || last.Status != domain.CreditNoteIssued {
		t.Errorf("last draft: status %q err %v, want issued", last.Status, err)
	}
}

// Walking the clock's list page by page, as the job does, returns every held
// note exactly once when the order notes are touched in disagrees with the
// order they were created in: the first-created notes were touched last.
// The page's order and the cursor must both be (created_at, id); a page
// ordered by updated_at would end on a late-created note and leave the
// earlier ones behind the cursor, and a cursor on updated_at would meet
// notes again.
func TestListPendingClawbackDraftsForClock_WalkReturnsEveryNoteOnce(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Clock Relief Walk")
	const n, page = 5, 2
	clockID, notes := seedClockReliefDrafts(t, db, ctx, tenantID, n)
	base := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, cn := range notes {
		updated := base.Add(time.Duration(i) * time.Minute)
		if i < 3 {
			updated = updated.Add(time.Hour) // touched last
		}
		execBypassCN(t, db, `UPDATE credit_notes SET created_at = $2, updated_at = $3 WHERE id = $1`,
			cn.ID, base.Add(time.Duration(i)*time.Minute), updated)
	}
	store := creditnote.NewPostgresStore(db)

	seen := map[string]int{}
	var afterCreatedAt time.Time
	afterID := ""
	for range n + 1 {
		got, err := store.ListPendingClawbackDraftsForClock(ctx, tenantID, clockID, afterCreatedAt, afterID, page)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, cn := range got {
			seen[cn.ID]++
		}
		if len(got) < page {
			break
		}
		afterCreatedAt, afterID = got[len(got)-1].CreatedAt, got[len(got)-1].ID
	}
	for i, cn := range notes {
		if seen[cn.ID] != 1 {
			t.Errorf("note %d (created %d of %d) returned %d times, want once", i, i+1, n, seen[cn.ID])
		}
	}
}

// refusingGranter refuses the credit grant a relief note on a paid invoice
// makes, so the issue rolls back and the note stays held.
type refusingGranter struct{ okGranterTx }

func (refusingGranter) GrantForCreditNoteTx(context.Context, *sql.Tx, string, string, creditnote.CreditGrantInput) error {
	return errors.New("credit grant refused")
}

// A note whose issue fails stays held and stays in the list. The cursor moves
// past it, so each note is tried once and the job ends with one error per
// note.
func TestRetryPendingClawbackIssueForClock_NotesThatStayListed(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Clock Relief Stay Listed")
	const n = 2*100 + 1
	clockID, _ := seedClockReliefDrafts(t, db, ctx, tenantID, n)

	svc := creditnote.NewService(creditnote.NewPostgresStore(db), invoice.NewPostgresStore(db), nil, refusingGranter{})
	runCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	done := make(chan []error, 1)
	go func() {
		_, errs := svc.RetryPendingClawbackIssueForClock(runCtx, tenantID, clockID, time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC), 100)
		done <- errs
	}()
	select {
	case errs := <-done:
		if len(errs) != n {
			t.Errorf("errors = %d, want %d: one per note, each note tried once", len(errs), n)
		}
	case <-runCtx.Done():
		t.Fatal("the job did not end: it read the held notes again")
	}
}
