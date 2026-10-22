package billing_test

import (
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// A cancel decides its final invoice against the pause in force at the
// cancel instant: a keep_as_draft pause leaves the final invoice a draft, and
// nothing charges it. The pause stays on the canceled row: it still holds the
// subscription's unpaid invoices, this final draft among them, until an
// operator resumes collection (TestCanceledPausedSub_PauseStillHoldsItsInvoices
// in internal/invoice covers the sweep and the tax retry).
//
// Mutation: clear the pause in the cancel and both cases fail.
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
			if sub.PauseCollection == nil {
				t.Error("the cancel cleared the pause: it still holds the subscription's unpaid invoices, this final draft among them")
			}
		})
	}
}
