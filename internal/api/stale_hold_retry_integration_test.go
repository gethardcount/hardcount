package api

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/dunning"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// The dunning retry and the charge hold (#976, ADR-127), on the real stores
// and the production adapter. Only Stripe is faked.

func (r *hardDeclineRig) holdAt(id string) *time.Time {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var at sql.NullTime
	if err := tx.QueryRowContext(r.ctx, `SELECT auto_charge_claimed_until FROM invoices WHERE id = $1`, id).Scan(&at); err != nil {
		r.t.Fatalf("read hold: %v", err)
	}
	if !at.Valid {
		return nil
	}
	return &at.Time
}

func (r *hardDeclineRig) setStaleHold(id string) {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(r.ctx, `UPDATE invoices SET auto_charge_claimed_until = now() - interval '11 minutes' WHERE id = $1`, id); err != nil {
		r.t.Fatalf("set hold: %v", err)
	}
	if err := tx.Commit(); err != nil {
		r.t.Fatalf("commit: %v", err)
	}
}

// A charge whose result was lost left a stale hold: the dunning retry charges
// nothing and skips uncounted (ErrTransientSkip), and the hold stays for the
// stale-hold sweep.
func TestDunningRetry_StaleHold_SkippedAndNothingCharged_RealStore(t *testing.T) {
	r := newHardDeclineRig(t, "Retry Stale Hold")
	custID := r.customerWithCard("stale", "pm_stale")
	inv := r.finalizedInvoice(custID, "INV-RETRY-STALE")
	r.setStaleHold(inv.ID)
	r.charges.answers = []scriptedCharge{{pi: "pi_second"}}

	err := r.retrier.RetryPayment(r.ctx, r.tenantID, inv.ID, custID)
	if !errors.Is(err, dunning.ErrTransientSkip) {
		t.Fatalf("err = %v, want ErrTransientSkip (uncounted)", err)
	}
	if n := len(r.charges.charged); n != 0 {
		t.Fatalf("Stripe received %d charges for an invoice whose earlier charge may have taken the money", n)
	}
	if r.holdAt(inv.ID) == nil {
		t.Fatal("the retry cleared a stale hold: only the stale-hold sweep may")
	}
}

// Credits cover the whole invoice: the retry settles it with no charge and
// releases its hold.
func TestDunningRetry_CreditCovered_SettledAndHoldReleased_RealStore(t *testing.T) {
	r := newHardDeclineRig(t, "Retry Credit Covered")
	custID := r.customerWithCard("covered", "pm_covered")
	inv := r.finalizedInvoice(custID, "INV-RETRY-COVERED")
	credits := credit.NewService(credit.NewPostgresStore(r.db))
	if _, err := credits.Grant(r.ctx, r.tenantID, credit.GrantInput{Currency: "USD", CustomerID: custID, AmountCents: 5000, Description: "goodwill"}); err != nil {
		t.Fatalf("grant credit: %v", err)
	}
	retrier := &paymentRetrierAdapter{charger: r.charger, invoiceStore: r.invoices, paymentSetups: r.retrier.paymentSetups, credits: credits, stripeReady: stripeConnectedForTest{connected: true}}

	if err := retrier.RetryPayment(r.ctx, r.tenantID, inv.ID, custID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if n := len(r.charges.charged); n != 0 {
		t.Fatalf("Stripe received %d charges for an invoice credits covered", n)
	}
	if got := r.invoice(inv.ID); got.Status != domain.InvoicePaid {
		t.Fatalf("status %s, want paid", got.Status)
	}
	if at := r.holdAt(inv.ID); at != nil {
		t.Fatalf("charge hold still set (%v) after a settle that sent no charge", *at)
	}
}
