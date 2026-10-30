package creditnote

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// Release plan row 13: finalize issues the relief held on a draft on its own
// transaction (IssueHeldReliefTx), so the invoice is chargeable only at the
// relieved amount. These pin the issuing rules; the real-Postgres e2e
// (api TestE2E_HeldReliefIssuesAtFinalize) pins the finalize wiring.

func heldNote(id string, cents int64, at time.Time) domain.CreditNote {
	return domain.CreditNote{
		ID: id, TenantID: "t1", InvoiceID: "inv_1", CustomerID: "cus_1", CreditNoteNumber: "CN-" + id,
		Status: domain.CreditNoteDraft, IssuePending: true, TotalCents: cents, Currency: "USD", CreatedAt: at,
	}
}

func finalizedOwing(cents int64) domain.Invoice {
	return domain.Invoice{
		ID: "inv_1", TenantID: "t1", CustomerID: "cus_1", InvoiceNumber: "INV-1", Currency: "USD",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		TotalAmountCents: cents, AmountDueCents: cents, TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
	}
}

func TestIssueHeldReliefTx_EachNoteComesOffWhatIsDue(t *testing.T) {
	store := newMemStore()
	t0 := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	store.notes["a"] = heldNote("a", 1419, t0)
	store.notes["b"] = heldNote("b", 300, t0.Add(time.Minute))
	inv := finalizedOwing(5000)
	invoices := &memInvoiceReader{invoices: map[string]domain.Invoice{inv.ID: inv}}
	svc := NewService(store, invoices, nil)

	issued, err := svc.IssueHeldReliefTx(context.Background(), nil, "t1", inv)
	if err != nil {
		t.Fatalf("IssueHeldReliefTx: %v", err)
	}
	if len(issued) != 2 {
		t.Fatalf("issued %d notes, want 2", len(issued))
	}
	if due := invoices.invoices["inv_1"].AmountDueCents; due != 5000-1419-300 {
		t.Fatalf("amount due %d, want %d", due, 5000-1419-300)
	}
	for _, id := range []string{"a", "b"} {
		if st := store.notes[id].Status; st != domain.CreditNoteIssued {
			t.Errorf("note %s is %s, want issued", id, st)
		}
	}
	// The reconciler's later pass cannot apply them again.
	if _, err := svc.Issue(context.Background(), "t1", "a"); err == nil {
		t.Fatal("Issue on an issued note succeeded; want the draft-only refusal")
	}
	if due := invoices.invoices["inv_1"].AmountDueCents; due != 5000-1419-300 {
		t.Fatalf("amount due moved to %d after the reconciler's pass", due)
	}
}

// casLostStore loses every draft→issued claim, as when another issuer won it.
type casLostStore struct{ *memStore }

func (casLostStore) TransitionStatusTx(context.Context, *sql.Tx, string, string, domain.CreditNoteStatus, domain.CreditNoteStatus) (bool, error) {
	return false, nil
}

func TestIssueHeldReliefTx_ALostClaimIsNotApplied(t *testing.T) {
	store := newMemStore()
	store.notes["a"] = heldNote("a", 1419, time.Now())
	inv := finalizedOwing(5000)
	invoices := &memInvoiceReader{invoices: map[string]domain.Invoice{inv.ID: inv}}
	svc := NewService(casLostStore{store}, invoices, nil)

	issued, err := svc.IssueHeldReliefTx(context.Background(), nil, "t1", inv)
	if err != nil {
		t.Fatalf("IssueHeldReliefTx: %v", err)
	}
	if len(issued) != 0 || invoices.invoices["inv_1"].AmountDueCents != 5000 {
		t.Fatalf("issued %d, due %d: a note whose claim was lost must not be applied", len(issued), invoices.invoices["inv_1"].AmountDueCents)
	}
}

// A note that leaves nothing due marks the invoice paid; the rest stay held
// for the reconciler, which issues them on the paid branch.
func TestIssueHeldReliefTx_StopsOnceTheInvoiceIsPaid(t *testing.T) {
	store := newMemStore()
	t0 := time.Now()
	store.notes["a"] = heldNote("a", 5000, t0)
	store.notes["b"] = heldNote("b", 300, t0.Add(time.Minute))
	inv := finalizedOwing(5000)
	invoices := &memInvoiceReader{invoices: map[string]domain.Invoice{inv.ID: inv}}
	svc := NewService(store, invoices, nil)

	issued, err := svc.IssueHeldReliefTx(context.Background(), nil, "t1", inv)
	if err != nil {
		t.Fatalf("IssueHeldReliefTx: %v", err)
	}
	if len(issued) != 1 || !issued[0].Settled {
		t.Fatalf("issued = %+v, want only the first note, settling the invoice", issued)
	}
	if got := invoices.invoices["inv_1"]; got.Status != domain.InvoicePaid || got.AmountDueCents != 0 {
		t.Fatalf("invoice %s with %d due, want paid with 0", got.Status, got.AmountDueCents)
	}
	if st := store.notes["b"].Status; st != domain.CreditNoteDraft {
		t.Fatalf("second note is %s, want still a held draft", st)
	}
}
