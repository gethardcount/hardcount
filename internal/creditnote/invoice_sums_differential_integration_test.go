package creditnote_test

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// defaultPage is how many notes the store list returns when asked for no
// limit, the page the old sums read.
const defaultPage = 50

// Under one page, the sums equal what the old page-and-add code returned.
// Notes are written straight to the table with every status, refund amount
// and issue stamp, on several invoices, so the filters are checked against
// each other on shapes the service does not produce today too (a voided note
// that was issued).
func TestInvoiceSums_MatchThePagedSumsUnderAPage(t *testing.T) {
	r := newSumsRig(t, "CN Sums Differential")
	seed := time.Now().UnixNano()
	rng := rand.New(rand.NewSource(seed))
	t.Logf("seed %d", seed)

	cust, err := customer.NewPostgresStore(r.db).Create(r.ctx, r.tenantID, domain.Customer{ExternalID: "cus_sums_diff", DisplayName: "Sums"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	statuses := []domain.CreditNoteStatus{domain.CreditNoteDraft, domain.CreditNoteIssued, domain.CreditNoteVoided}
	var invoiceIDs []string
	n := 0
	for i := range 8 {
		now := time.Now().UTC()
		inv, err := r.invoices.Create(r.ctx, r.tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: fmt.Sprintf("INV-SUMS-%d", i),
			Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending, Currency: "USD",
			SubtotalCents: 1_000_000, TotalAmountCents: 1_000_000, AmountDueCents: 1_000_000,
			BillingPeriodStart: now.Add(-720 * time.Hour), BillingPeriodEnd: now, IssuedAt: &now,
		})
		if err != nil {
			t.Fatalf("create invoice: %v", err)
		}
		invoiceIDs = append(invoiceIDs, inv.ID)

		tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
		if err != nil {
			t.Fatal(err)
		}
		notes := rng.Intn(defaultPage) // 0..49
		if i == 0 {
			notes = 0
		}
		for range notes {
			n++
			st := statuses[rng.Intn(len(statuses))]
			var issuedAt *time.Time
			if st == domain.CreditNoteIssued || (st == domain.CreditNoteVoided && rng.Intn(2) == 0) {
				issuedAt = &now
			}
			var refund int64
			if rng.Intn(2) == 0 {
				refund = rng.Int63n(5000)
			}
			if _, err := tx.ExecContext(r.ctx, `
				INSERT INTO credit_notes (tenant_id, invoice_id, customer_id, credit_note_number, status, reason,
					total_cents, refund_amount_cents, currency, issued_at)
				VALUES ($1, $2, $3, $4, $5, 'diff', $6, $7, 'USD', $8)`,
				r.tenantID, inv.ID, cust.ID, fmt.Sprintf("CN-SUMS-%d", n), st,
				1+rng.Int63n(10_000), refund, postgres.NullableTime(issuedAt)); err != nil {
				t.Fatalf("insert note: %v", err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	store := creditnote.NewPostgresStore(r.db)
	for _, id := range invoiceIDs {
		notes, err := store.List(r.ctx, creditnote.ListFilter{TenantID: r.tenantID, InvoiceID: id, Limit: 100})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(notes) >= defaultPage {
			t.Fatalf("invoice %s has %d notes; the old sums are only right under one page", id, len(notes))
		}
		// The old code, verbatim in its filters.
		var oldCredited, oldIssued, oldUnrelieved, oldRefunds int64
		for _, cn := range notes {
			if cn.Status != domain.CreditNoteVoided {
				oldCredited += cn.TotalCents
			}
			if cn.Status == domain.CreditNoteIssued {
				oldIssued += cn.TotalCents
			}
			if cn.Status == domain.CreditNoteVoided && cn.IssuedAt == nil {
				oldUnrelieved += cn.TotalCents
			}
			if cn.Status != domain.CreditNoteVoided && cn.RefundAmountCents > 0 {
				oldRefunds += cn.RefundAmountCents
			}
		}

		credited, err := r.svc.CreditedCents(r.ctx, r.tenantID, id)
		if err != nil {
			t.Fatal(err)
		}
		issued, err := r.svc.IssuedCreditedCents(r.ctx, r.tenantID, id)
		if err != nil {
			t.Fatal(err)
		}
		unrelieved, err := r.svc.UnreliefedClawbackCents(r.ctx, r.tenantID, id)
		if err != nil {
			t.Fatal(err)
		}
		sums, err := store.InvoiceSums(r.ctx, r.tenantID, id)
		if err != nil {
			t.Fatal(err)
		}
		if credited != oldCredited || issued != oldIssued || unrelieved != oldUnrelieved || sums.RefundCents != oldRefunds {
			t.Errorf("invoice %s (%d notes): credited %d/%d issued %d/%d unrelieved %d/%d refunds %d/%d (new/old)",
				id, len(notes), credited, oldCredited, issued, oldIssued, unrelieved, oldUnrelieved, sums.RefundCents, oldRefunds)
		}
	}
}
