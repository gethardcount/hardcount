package invoice

import (
	"database/sql"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/subscription"
)

// TestCanceledPausedSub_PauseStillHoldsItsInvoices: a cancel ends the
// subscription, not the pause on its unpaid invoices. Dunning's `pause` final
// action on an invoice with no card, then an operator cancel, is the real
// trigger. The canceled subscription's queued invoice must stay out of the
// auto-charge sweep (which is also what charges once the customer adds a
// card), and a keep_as_draft draft whose tax was pending must stay a draft
// when its tax retry succeeds. Resume is how the operator releases them.
//
// Mutation: clear the pause in any cancel writer and the sweep lists the
// queued invoice and the retry finalizes the draft.
func TestCanceledPausedSub_PauseStillHoldsItsInvoices(t *testing.T) {
	r := newSnapRig(t, "Canceled Paused Sub Corp")
	draftID := r.subTaxPendingDraft(t, "A4-HOLD-DRAFT", true)
	draft, err := r.store.Get(r.ctx, r.tenantID, draftID)
	if err != nil {
		t.Fatalf("get draft: %v", err)
	}
	subID := draft.SubscriptionID

	q := r.protoInvoice("A4-HOLD-QUEUED", domain.InvoiceFinalized, domain.BillingReasonSubscriptionCycle)
	q.SubscriptionID = subID
	queued, err := r.store.CreateWithLineItems(r.ctx, r.tenantID, q, nil)
	if err != nil {
		t.Fatalf("create queued invoice: %v", err)
	}
	if err := r.db.WithTenantTx(r.ctx, r.tenantID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.ctx, `UPDATE invoices SET auto_charge_pending = TRUE WHERE id = $1`, queued.ID)
		return err
	}); err != nil {
		t.Fatalf("queue invoice for auto-charge: %v", err)
	}

	subs := subscription.NewPostgresStore(r.db)
	canceled, err := subs.CancelAtomic(r.ctx, r.tenantID, subID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if canceled.PauseCollection == nil {
		t.Error("the cancel cleared the pause: it still holds the subscription's unpaid invoices")
	}

	if r.sweepLists(t, queued.ID) {
		t.Error("the auto-charge sweep lists the canceled subscription's queued invoice while collection is paused")
	}

	r.svc.SetTaxRetrier(&resolvingTaxRetrier{db: r.db, store: r.store})
	if _, err := r.svc.RetryTax(r.ctx, r.tenantID, draftID); err != nil {
		t.Fatalf("RetryTax: %v", err)
	}
	got, err := r.store.Get(r.ctx, r.tenantID, draftID)
	if err != nil {
		t.Fatalf("get draft after retry: %v", err)
	}
	if got.Status != domain.InvoiceDraft || got.AutoChargePending {
		t.Errorf("after the tax retry: status = %s, auto_charge_pending = %v; want a draft, not queued (keep_as_draft)", got.Status, got.AutoChargePending)
	}

	// Negative control: resuming collection releases the queued invoice, so
	// the sweep check above can see it.
	if _, err := subs.ClearPauseCollection(r.ctx, r.tenantID, subID); err != nil {
		t.Fatalf("resume collection: %v", err)
	}
	if !r.sweepLists(t, queued.ID) {
		t.Error("after a resume the sweep still skips the queued invoice: the check above proves nothing")
	}
}

// sweepLists pages through the whole auto-charge queue and reports whether
// it holds id.
func (r *snapRig) sweepLists(t *testing.T, id string) bool {
	t.Helper()
	var after domain.InvoiceKeyset
	for {
		page, err := r.store.ListAutoChargePending(r.ctx, after, 200)
		if err != nil {
			t.Fatalf("ListAutoChargePending: %v", err)
		}
		for _, inv := range page {
			if inv.ID == id {
				return true
			}
		}
		if len(page) < 200 {
			return false
		}
		after = domain.KeysetOf(page[len(page)-1])
	}
}
