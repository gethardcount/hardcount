package creditnote_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestStats_CountsEveryNoteAndKeepsCurrenciesApart (E12): the Credit Notes
// page's header, tabs and stat cards were computed from the rows it had
// loaded — at most 100, and only the open tab's status — so a tenant past 100
// notes saw wrong totals, and the tab counts vanished under a filter. Stats
// counts every note in the tenant and mode, and sums money per currency,
// never across currencies.
//
// "Refunded" counts only refund legs that succeeded: a refund amount on a
// note is an instruction to move cash, and only refund_status says whether
// cash moved (observed live 2026-08-03: failed legs shown as returned money).
//
// Mutation-verify: add LIMIT 100 to the count query — draft_count reads 100;
// drop the currency GROUP BY — the EUR figures land in USD.
func TestStats_CountsEveryNoteAndKeepsCurrenciesApart(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	tenantID := testutil.CreateTestTenant(t, db, "CN stats")
	custStore := customer.NewPostgresStore(db)
	invStore := invoice.NewPostgresStore(db)
	cnStore := creditnote.NewPostgresStore(db)

	cust, err := custStore.Create(ctx, tenantID, domain.Customer{ExternalID: "cus-cn-stats", DisplayName: "CN stats"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	now := time.Now().UTC()
	mkInvoice := func(num, currency string) domain.Invoice {
		inv, err := invStore.Create(ctx, tenantID, domain.Invoice{
			CustomerID:         cust.ID,
			InvoiceNumber:      num,
			Status:             domain.InvoicePaid,
			PaymentStatus:      domain.PaymentSucceeded,
			Currency:           currency,
			SubtotalCents:      1_000_000,
			TotalAmountCents:   1_000_000,
			AmountPaidCents:    1_000_000,
			BillingPeriodStart: now.Add(-30 * 24 * time.Hour),
			BillingPeriodEnd:   now,
			IssuedAt:           &now,
		})
		if err != nil {
			t.Fatalf("create invoice %s: %v", num, err)
		}
		return inv
	}
	usd := mkInvoice("INV-CNS-USD", "USD")
	eur := mkInvoice("INV-CNS-EUR", "EUR")

	seq := 0
	mkNote := func(inv domain.Invoice, status domain.CreditNoteStatus, refundStatus domain.RefundStatus, refund, credit, oob int64) {
		t.Helper()
		seq++
		_, err := cnStore.CreateUnderInvoiceLock(ctx, tenantID, inv.ID, nil,
			func(*sql.Tx, []domain.CreditNote) (domain.CreditNote, error) {
				return domain.CreditNote{
					InvoiceID:            inv.ID,
					CustomerID:           cust.ID,
					CreditNoteNumber:     fmt.Sprintf("CN-STATS-%04d", seq),
					Status:               status,
					Reason:               "stats fixture",
					SubtotalCents:        refund + credit + oob,
					TotalCents:           refund + credit + oob,
					RefundAmountCents:    refund,
					CreditAmountCents:    credit,
					OutOfBandAmountCents: oob,
					Currency:             inv.Currency,
					RefundStatus:         refundStatus,
				}, nil
			})
		if err != nil {
			t.Fatalf("create note %d: %v", seq, err)
		}
	}

	// USD, issued: one of each refund outcome, a balance credit, an
	// out-of-band leg.
	mkNote(usd, domain.CreditNoteIssued, domain.RefundSucceeded, 1000, 0, 0)
	mkNote(usd, domain.CreditNoteIssued, domain.RefundFailed, 200, 0, 0)
	mkNote(usd, domain.CreditNoteIssued, domain.RefundPending, 300, 0, 0)
	mkNote(usd, domain.CreditNoteIssued, domain.RefundNone, 0, 400, 50)
	// A note split two ways: its credit leg settled at issue, its card leg
	// failed. Each leg lands in its own figure.
	mkNote(usd, domain.CreditNoteIssued, domain.RefundFailed, 100, 150, 0)
	// EUR, issued.
	mkNote(eur, domain.CreditNoteIssued, domain.RefundSucceeded, 700, 0, 0)
	mkNote(eur, domain.CreditNoteIssued, domain.RefundNone, 0, 90, 0)
	// Voided and draft notes are counted, but they moved no money.
	mkNote(usd, domain.CreditNoteVoided, domain.RefundNone, 0, 5000, 0)
	for i := 0; i < 101; i++ {
		mkNote(usd, domain.CreditNoteDraft, domain.RefundNone, 0, 10, 0)
	}

	got, err := cnStore.Stats(ctx, tenantID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if got.DraftCount != 101 || got.IssuedCount != 7 || got.VoidedCount != 1 {
		t.Errorf("counts = draft %d, issued %d, voided %d; want 101, 7, 1 (every note, not the first 100)",
			got.DraftCount, got.IssuedCount, got.VoidedCount)
	}

	want := map[string]creditnote.CurrencyStats{
		"USD": {Currency: "USD", TotalCents: 2200, RefundedCents: 1000, RefundPendingCents: 300, RefundFailedCents: 300, CreditedCents: 550, OutOfBandCents: 50},
		"EUR": {Currency: "EUR", TotalCents: 790, RefundedCents: 700, CreditedCents: 90},
	}
	if len(got.ByCurrency) != len(want) {
		t.Fatalf("by_currency = %+v, want one entry each for USD and EUR", got.ByCurrency)
	}
	// The tenant's default currency (USD) comes first.
	if got.ByCurrency[0].Currency != "USD" {
		t.Errorf("first currency = %s, want the default USD first", got.ByCurrency[0].Currency)
	}
	for _, c := range got.ByCurrency {
		if c != want[c.Currency] {
			t.Errorf("%s:\n  got  %+v\n  want %+v", c.Currency, c, want[c.Currency])
		}
	}

	// The other mode sees none of these notes.
	live, err := cnStore.Stats(postgres.WithLivemode(context.Background(), true), tenantID)
	if err != nil {
		t.Fatalf("live stats: %v", err)
	}
	if live.DraftCount+live.IssuedCount+live.VoidedCount != 0 || len(live.ByCurrency) != 0 {
		t.Errorf("live-mode stats = %+v, want empty: test-mode notes leaked across modes", live)
	}
}
