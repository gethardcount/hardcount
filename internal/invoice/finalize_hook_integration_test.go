package invoice

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
)

// A finalize hook that fails rolls the flip back on the real store: the
// invoice stays a draft and is not queued for a charge (release plan row 13:
// held relief that cannot be issued must not leave a full-price invoice
// chargeable).
func TestFinalizeDraft_HookFailureRollsTheFlipBack(t *testing.T) {
	r := newSnapRig(t, "Finalize Hook Corp")
	p := r.protoInvoice("HOOK-1", domain.InvoiceDraft, domain.BillingReasonSubscriptionCycle)
	p.IssuedAt = nil
	d, err := r.store.CreateWithLineItems(r.ctx, r.tenantID, p, nil)
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	_, err = r.store.FinalizeDraft(r.ctx, r.tenantID, d.ID, func(*sql.Tx, domain.Invoice) error {
		return errors.New("credit note exceeds invoice")
	})
	if err == nil {
		t.Fatal("FinalizeDraft succeeded although its hook failed")
	}
	got, err := r.store.Get(r.ctx, r.tenantID, d.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.InvoiceDraft || got.AutoChargePending {
		t.Fatalf("status %s, auto_charge_pending %v: want a draft not queued for a charge", got.Status, got.AutoChargePending)
	}

	// A hook that lowers what is due on the same tx: the returned row carries it.
	inv, err := r.store.FinalizeDraft(r.ctx, r.tenantID, d.ID, func(tx *sql.Tx, flipped domain.Invoice) error {
		_, err := r.store.ApplyCreditNoteTx(r.ctx, tx, r.tenantID, flipped.ID, 1419, 0)
		return err
	})
	if err != nil {
		t.Fatalf("FinalizeDraft: %v", err)
	}
	if inv.Status != domain.InvoiceFinalized || inv.AmountDueCents != 5000-1419 {
		t.Fatalf("finalized %s with %d due, want finalized with %d", inv.Status, inv.AmountDueCents, 5000-1419)
	}
}
