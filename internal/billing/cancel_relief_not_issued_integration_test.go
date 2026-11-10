package billing_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/gethardcount/hardcount/internal/billing"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// failingFirstIssue fails the first Issue of a relief credit note, as a
// database error or a deadline would, and passes every later call through.
type failingFirstIssue struct {
	billing.CreditNoteAdjuster
	mu     sync.Mutex
	failed bool
}

func (f *failingFirstIssue) Issue(ctx context.Context, tenantID, id string) (domain.CreditNote, error) {
	f.mu.Lock()
	first := !f.failed
	f.failed = true
	f.mu.Unlock()
	if first {
		return domain.CreditNote{}, errors.New("issue: database is shutting down")
	}
	return f.CreditNoteAdjuster.Issue(ctx, tenantID, id)
}

// A cancel's relief credit note is not issued in the call (the issuer
// fails). Collected in the same call, the final invoice charged the card in
// full, 3000c, and the 2450c relief landed on the balance of a customer with
// no subscription. Now the final invoice is not collected in the call: the
// billing run issues the note (clawback_issue), then collects queued invoices
// (the auto-charge step), and the card is charged 550c, net of the relief.
//
// The rig is the #1272 one: a 4900c in-advance base, paid, canceled half way
// through the month (2450c of relief), and 3000c of usage on the final
// invoice. Card on file.
func TestCancel_ReliefNotIssued_FinalInvoiceWaitsForTheSweeps(t *testing.T) {
	db := testutil.SetupTestDB(t)
	for _, c := range []struct {
		name string
		run  func(*reliefCancelRig, *testing.T)
	}{
		{"immediate cancel", (*reliefCancelRig).cancelImmediately},
		{"scheduled cancel", (*reliefCancelRig).fireScheduledCancel},
	} {
		t.Run(c.name, func(t *testing.T) {
			const usageCents, wantDue = 3000, 3000 - reliefCents
			r := newReliefCancelRig(t, db, "Relief Not Issued "+c.name, reliefRigOpts{card: true, usageUnits: usageCents})
			r.engine.SetCreditNoteAdjuster(&failingFirstIssue{CreditNoteAdjuster: r.cn})
			c.run(r, t)

			final := r.finalInvoice(t)
			if charges := r.charger.charges(final.ID); len(charges) != 0 {
				t.Fatalf("final invoice charged %v in the cancel's own call, want nothing: the relief was not issued, so the card would pay all %dc", charges, usageCents)
			}
			if final.AmountDueCents != usageCents || final.Status != domain.InvoiceFinalized {
				t.Fatalf("after the cancel: final invoice %s with %dc due, want finalized with %dc due (not collected yet)", final.Status, final.AmountDueCents, usageCents)
			}

			// The billing run, in its order: issue relief drafts, then collect.
			if n, errs := r.cn.RetryPendingClawbackIssue(r.ctx, 50); n != 1 || len(errs) != 0 {
				t.Fatalf("clawback_issue: issued=%d errs=%v, want the relief note issued", n, errs)
			}
			if _, errs := r.engine.RetryPendingCharges(r.ctx, 50, nil); len(errs) != 0 {
				t.Fatalf("auto-charge step: %v", errs)
			}

			final = r.invoice(t, final.ID)
			if final.CreditsAppliedCents != reliefCents {
				t.Errorf("final invoice credits_applied = %dc, want %dc: the relief must reduce it", final.CreditsAppliedCents, reliefCents)
			}
			if charges := r.charger.charges(final.ID); len(charges) != 1 || charges[0] != wantDue {
				t.Errorf("final invoice charges = %v, want one charge of %dc, net of the relief", charges, wantDue)
			}
			if bal := r.balance(t); bal != 0 {
				t.Errorf("credit balance = %dc, want 0 (the relief was spent on the final invoice)", bal)
			}
		})
	}
}
