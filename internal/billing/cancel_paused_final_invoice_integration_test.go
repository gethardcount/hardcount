package billing_test

import (
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// The money guard on the cancel's clear of the pause (A4). A cancel decides
// its final invoice against the pause in force at the cancel instant: a
// keep_as_draft pause leaves the final invoice a draft, and nothing charges
// it. The cancel then clears the pause from the canceled row, so the row and
// the API stop saying collection is paused on a subscription that has ended.
//
// The clear must be the cancel transaction's last write. The final invoice is
// built from the canceled row the flip returns; clearing the pause in the flip
// itself would finalize that invoice and charge the card on file.
//
// Mutation: move retireLiveOnlyStateTx before billFn (or into the flip's
// UPDATE) and both cases fail.
func TestCancel_PausedSub_FinalInvoiceStaysDraft(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	db := testutil.SetupTestDB(t)

	cancels := []struct {
		name string
		run  func(*reliefCancelRig, *testing.T)
	}{
		{"immediate cancel", (*reliefCancelRig).cancelImmediately},
		{"scheduled cancel", (*reliefCancelRig).fireScheduledCancel},
	}
	for _, c := range cancels {
		t.Run(c.name, func(t *testing.T) {
			r := newReliefCancelRig(t, db, "Paused Cancel "+c.name, reliefRigOpts{card: true, usageUnits: 3000})
			store := subscription.NewPostgresStore(db)
			if _, err := store.SetPauseCollection(r.ctx, r.tenantID, r.subID, domain.PauseCollection{
				Behavior: domain.PauseCollectionKeepAsDraft,
			}); err != nil {
				t.Fatalf("pause collection: %v", err)
			}

			c.run(r, t)

			final := r.finalInvoice(t)
			if final.Status != domain.InvoiceDraft {
				t.Errorf("final invoice is %s, want draft: collection was paused (keep_as_draft) when the subscription was canceled", final.Status)
			}
			if got := r.charger.charges(final.ID); len(got) != 0 {
				t.Errorf("final invoice charged %v while collection was paused", got)
			}
			sub, err := store.Get(r.ctx, r.tenantID, r.subID)
			if err != nil {
				t.Fatalf("read subscription: %v", err)
			}
			if sub.PauseCollection != nil {
				t.Errorf("canceled subscription still carries pause %+v: a canceled subscription has no collection to resume", *sub.PauseCollection)
			}
		})
	}
}
