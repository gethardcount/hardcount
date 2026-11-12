package creditnote_test

import (
	"context"
	"testing"

	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// expiryRecorder records the invoices whose Checkout sessions Issue expired.
type expiryRecorder struct{ invoices []string }

func (e *expiryRecorder) ExpireCheckoutSessions(_ context.Context, _ string, inv domain.Invoice) {
	e.invoices = append(e.invoices, inv.ID)
}

// A credit note that lowers what an unpaid invoice asks for expires the
// customer's open Checkout page, which still asks for the old amount; the
// next page asks for the new one. A note on a paid invoice lowers nothing
// that is owed, so it expires nothing.
//
// Mutations: drop the expiry from Issue → the first two cases are red; drop
// the dueReduced > 0 gate → the paid case is red.
func TestIssue_ExpiresCheckoutWhenItLowersWhatIsOwed(t *testing.T) {
	issue := func(t *testing.T, r *overCreditRig, inv domain.Invoice, cents int64) {
		t.Helper()
		cn, err := r.notes.Create(r.ctx, r.tenantID, creditnote.CreateInput{
			InvoiceID: inv.ID,
			Reason:    "order_change",
			Lines:     []creditnote.CreditLineInput{{Description: "credit", Quantity: 1, UnitAmountCents: cents}},
		})
		if err != nil {
			t.Fatalf("create credit note: %v", err)
		}
		if _, err := r.notes.Issue(r.ctx, r.tenantID, cn.ID); err != nil {
			t.Fatalf("issue credit note: %v", err)
		}
	}

	t.Run("part of what is owed", func(t *testing.T) {
		r := newOverCreditRig(t, "CN Checkout Expiry Part")
		rec := &expiryRecorder{}
		r.notes.SetCheckoutExpirer(rec)
		issue(t, r, r.inv, 3000)
		if len(rec.invoices) != 1 || rec.invoices[0] != r.inv.ID {
			t.Errorf("Checkout expiries = %v, want one for %s", rec.invoices, r.inv.ID)
		}
	})

	t.Run("all of what is owed", func(t *testing.T) {
		r := newOverCreditRig(t, "CN Checkout Expiry All")
		rec := &expiryRecorder{}
		r.notes.SetCheckoutExpirer(rec)
		issue(t, r, r.inv, r.inv.AmountDueCents)
		if got := r.invoiceNow(t); got.Status != domain.InvoicePaid {
			t.Fatalf("invoice status = %s, want paid", got.Status)
		}
		if len(rec.invoices) != 1 || rec.invoices[0] != r.inv.ID {
			t.Errorf("Checkout expiries = %v, want one for %s", rec.invoices, r.inv.ID)
		}
	})

	t.Run("a paid invoice", func(t *testing.T) {
		r := newOverCreditRig(t, "CN Checkout Expiry Paid")
		rec := &expiryRecorder{}
		r.notes.SetCheckoutExpirer(rec)
		_, paid := seedInvoiceForCN(t, r.db, r.tenantID, "checkout-expiry-paid", domain.InvoicePaid, domain.PaymentSucceeded, "pi_checkout_expiry_paid")
		issue(t, r, paid, 3000)
		if len(rec.invoices) != 0 {
			t.Errorf("Checkout expiries = %v, want none: a note on a paid invoice lowers nothing owed", rec.invoices)
		}
	})
}

// endingResolver closes nothing; it ends the caller's context, as a request
// that times out right after the commit would.
type endingResolver struct{ endCaller context.CancelFunc }

func (r endingResolver) ResolveByInvoice(context.Context, string, string, domain.DunningResolution) error {
	r.endCaller()
	return nil
}

// claimRecorder notes, at the expiry, whether its context is live and the
// status of the invoice's checkout claim as another connection sees it.
type claimRecorder struct {
	t       *testing.T
	r       *overCreditRig
	claimID string
	live    []bool
	claim   []string
}

func (c *claimRecorder) ExpireCheckoutSessions(ctx context.Context, _ string, _ domain.Invoice) {
	c.live = append(c.live, ctx.Err() == nil)
	tx, err := c.r.db.BeginTx(c.r.ctx, postgres.TxBypass, "")
	if err != nil {
		c.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var status string
	if err := tx.QueryRowContext(c.r.ctx, `SELECT status FROM checkout_sessions WHERE id = $1`, c.claimID).Scan(&status); err != nil {
		c.t.Fatalf("read claim: %v", err)
	}
	c.claim = append(c.claim, status)
}

// Issue expires the page after its commit, on a context the caller cannot
// cancel: a request that ends right after the commit must not leave the page
// payable, and an expiry inside the transaction would run before the claim
// it closes is visible.
//
// Mutations: hand the expiry the caller's context → live is false; move the
// expiry above the commit → the claim is still open.
func TestIssue_CheckoutExpiryIsDetachedAndAfterTheCommit(t *testing.T) {
	r := newOverCreditRig(t, "CN Checkout Expiry Detached")
	bypassExec(t, r.ctx, r.db, `INSERT INTO checkout_sessions (id, tenant_id, invoice_id, livemode, amount_cents, currency, status, expires_at)
		VALUES ('hc_cs_detached', $1, $2, false, $3, 'USD', 'open', now() + interval '1 hour')`, r.tenantID, r.inv.ID, r.inv.AmountDueCents)

	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	rec := &claimRecorder{t: t, r: r, claimID: "hc_cs_detached"}
	r.notes.SetCheckoutExpirer(rec)
	// A note for everything due settles the invoice, so Issue closes its
	// dunning run after the commit and before the expiry; that is where the
	// caller's context ends.
	r.notes.SetDunningResolver(endingResolver{endCaller: cancel})

	cn, err := r.notes.Create(r.ctx, r.tenantID, creditnote.CreateInput{
		InvoiceID: r.inv.ID,
		Reason:    "order_change",
		Lines:     []creditnote.CreditLineInput{{Description: "credit", Quantity: 1, UnitAmountCents: r.inv.AmountDueCents}},
	})
	if err != nil {
		t.Fatalf("create credit note: %v", err)
	}
	if _, err := r.notes.Issue(ctx, r.tenantID, cn.ID); err != nil {
		t.Fatalf("issue credit note: %v", err)
	}
	if len(rec.live) != 1 || !rec.live[0] {
		t.Errorf("expiry calls with a live context = %v, want [true]: a call on an ended context leaves the page payable", rec.live)
	}
	if len(rec.claim) != 1 || rec.claim[0] == "open" {
		t.Errorf("claim status at the expiry = %v, want closed: the expiry ran before the commit", rec.claim)
	}
}
