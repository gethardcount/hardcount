package billing

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// RunBillingHalf runs one billing tick body, as the leader gate does, for the
// external real-Postgres tests (package billing_test).
var RunBillingHalf = (*Scheduler).runBillingHalf

// NewStripeTaxFake starts a Stripe Tax API double for the external
// real-Postgres tests. See stripeTaxFake.
var NewStripeTaxFake = newStripeTaxFake

// StripeTaxFakeRequest is one calculation request the fake recorded.
type StripeTaxFakeRequest = stripeTaxFakeRequest

// BillFinalOnCancel bills the final invoice of a canceled subscription, for
// the tests that check what that invoice bills. It prices with no transaction
// open, then writes on one, the steps PlanCancelBill and its writer run, but
// plans and writes no relief and re-checks no watermark. After the commit it
// counts the usage the invoice left unpriced, then runs
// FinalizeOnCreateInvoice. The invoice is empty when there is nothing to bill.
// Production code has no such method: a cancel bills through PlanCancelBill's
// writer, on the transaction that cancels the subscription (ADR-056, ADR-139).
func (e *Engine) BillFinalOnCancel(ctx context.Context, sub domain.Subscription) (domain.Invoice, error) {
	live, err := postgres.Livemode(ctx)
	if err != nil {
		return domain.Invoice{}, err
	}
	unpriced := unpricedUsage{}
	p, err := e.priceFinalOnCancel(ctx, sub, unpriced)
	if err != nil {
		return domain.Invoice{}, err
	}
	var inv domain.Invoice
	if err := e.subs.WithTenantTx(ctx, sub.TenantID, func(tx *sql.Tx) error {
		inv, err = e.writeFinalOnCancel(ctx, tx, sub, p)
		return err
	}); err != nil {
		return domain.Invoice{}, err
	}
	unpriced.record(live, sub)
	if inv.ID != "" {
		e.FinalizeOnCreateInvoice(ctx, sub, inv)
	}
	return inv, nil
}

// BillCancelOnTx plans a cancel's bill (PlanCancelBill, at canceled.CanceledAt)
// and runs its writer on tx with canceled as the locked row. It is for the
// tests that drive a cancel's bill on a transaction they hold open themselves.
// The plan's reads run while tx is open, which production never does: a
// cancel plans before its transaction (subscription.Service.cancel).
func (e *Engine) BillCancelOnTx(ctx context.Context, tx *sql.Tx, canceled domain.Subscription) (domain.Invoice, domain.Relief, func(), error) {
	if canceled.CanceledAt == nil {
		return domain.Invoice{}, domain.Relief{}, nil, errors.New("BillCancelOnTx: the subscription has no canceled_at")
	}
	write, err := e.PlanCancelBill(ctx, canceled, *canceled.CanceledAt)
	if err != nil {
		return domain.Invoice{}, domain.Relief{}, nil, err
	}
	return write(ctx, tx, canceled)
}

// WriteCancelReliefTx plans a cancel's relief from canceled and writes it on
// tx, for the tests that check what the relief writes. Production plans the
// relief in PlanCancelBill, before the cancel's transaction, and writes it in
// that transaction (writePlanned).
func (e *Engine) WriteCancelReliefTx(ctx context.Context, tx *sql.Tx, canceled domain.Subscription) (domain.Relief, error) {
	p, err := e.planCancelRelief(ctx, canceled)
	if err != nil {
		return domain.Relief{}, err
	}
	return e.writeReliefTx(ctx, tx, canceled, p)
}

// ShortenCommitBudget sets commitBudget to d for one test, so a test can run
// a transaction past its budget without waiting 30 seconds.
func ShortenCommitBudget(t *testing.T, d time.Duration) {
	t.Helper()
	prev := commitBudget
	commitBudget = d
	t.Cleanup(func() { commitBudget = prev })
}
