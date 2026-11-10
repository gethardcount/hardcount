package billing

import (
	"context"
	"database/sql"

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
// the tests that check what that invoice bills. It prices and writes on one
// transaction, the steps BillCancelTx runs, but writes no relief. After the
// commit it counts the usage the invoice left unpriced, then runs
// FinalizeOnCreateInvoice. The invoice is empty when there is nothing to bill.
// Production code has no such method: a cancel bills through BillCancelTx, on
// the transaction that cancels the subscription (ADR-056).
func (e *Engine) BillFinalOnCancel(ctx context.Context, sub domain.Subscription) (domain.Invoice, error) {
	live, err := postgres.Livemode(ctx)
	if err != nil {
		return domain.Invoice{}, err
	}
	unpriced := unpricedUsage{}
	var inv domain.Invoice
	if err := e.subs.WithTenantTx(ctx, sub.TenantID, func(tx *sql.Tx) error {
		p, err := e.priceFinalOnCancel(ctx, tx, sub, unpriced)
		if err != nil {
			return err
		}
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
