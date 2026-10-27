package api

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testclock"
)

// noBillingRunner is the catchup's billing seam with nothing to bill: the
// customer below has no subscription, so the cycle, threshold and charge
// phases find no work and the advance exercises dunning and credit expiry.
type noBillingRunner struct{}

func (noBillingRunner) RunCycleForClock(context.Context, string, string, int) (int, []error) {
	return 0, nil
}
func (noBillingRunner) RetryPendingChargesForClock(context.Context, string, string, int) (int, []error) {
	return 0, nil
}
func (noBillingRunner) ScanThresholdsForClock(context.Context, string, string, int) (int, []error) {
	return 0, nil
}

// TC-M2, real Postgres: the test-clock catchup's dunning retries and its
// credit expiry, with the production retry adapter and the real invoice,
// card, credit, dunning and test-clock stores. Only Stripe is scripted.
//
// The real trigger: an operator gives a customer in dunning a goodwill credit
// that expires in a week, then advances the clock a month. The retry on day 3
// must settle the invoice from the credit. Expiry used to run before the
// retries: it retired the grant at the advance target first, and the day-3
// retry charged the card in full.
//
// Which instant a retry tests a grant's expiry at (ADR-071 §2, amended
// 2026-10-20): the invoice here is a period invoice, so a retry judges the
// grant by the invoice's issued_at, the end of the period it bills, as the
// first collection does. The draw itself is still stamped at the retry.
func TestCatchup_CreditExpiryRunsAfterDunningRetries_RealStore(t *testing.T) {
	t0 := time.Date(2027, 5, 1, 0, 0, 0, 0, time.UTC) // the failed charge; dunning starts
	frozen := t0.AddDate(0, 0, 30)                    // the advance target
	firstRetry := t0.Add(72 * time.Hour)              // grace period 3 days

	cases := []struct {
		name         string
		grantedAt    time.Time
		expiresAt    time.Time
		creditPaysIt bool
	}{
		// Live at the first retry: that retry draws it and the invoice is paid.
		{"grant live at the first retry pays the invoice", t0, t0.AddDate(0, 0, 7), true},
		// Live when the period ended (issued_at), expired before the first
		// retry: the retry judges it by the period and draws it. Fails if a
		// retry tests expiry at its own instant, or if expiry runs first.
		{"grant live at the period end but expired before the first retry pays the invoice", t0, t0.AddDate(0, 0, 2), true},
		// Control: expired before the period ended, so no retry may draw it,
		// whatever the phase order. Running expiry last relies on this filter.
		{"grant expired before the period ended is not drawn", t0.AddDate(0, 0, -5), t0.AddDate(0, 0, -1), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newHardDeclineRig(t, "Catchup Credit Expiry")

			// The clock as Advance leaves it for the worker: marked advancing,
			// frozen_time already at the target.
			clockID := postgres.NewID("vlx_tclk")
			tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			if _, err := tx.ExecContext(r.ctx,
				`INSERT INTO test_clocks (id, tenant_id, livemode, name, frozen_time, status)
				 VALUES ($1, $2, false, 'Goodwill Credit', $3, 'advancing')`, clockID, r.tenantID, frozen); err != nil {
				postgres.Rollback(tx)
				t.Fatalf("insert clock: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit clock: %v", err)
			}

			cust, err := r.customers.Create(r.ctx, r.tenantID, domain.Customer{
				ExternalID: "ext_goodwill", DisplayName: "goodwill", Email: "goodwill@acme.test", TestClockID: clockID,
			})
			if err != nil {
				t.Fatalf("create customer: %v", err)
			}
			if err := r.customers.SetStripeCustomerID(r.ctx, r.tenantID, cust.ID, "cus_stripe_goodwill"); err != nil {
				t.Fatalf("stripe customer id: %v", err)
			}
			r.addCard(cust.ID, "pm_goodwill")

			dstore := dunning.NewPostgresStore(r.db)
			if _, err := dstore.UpsertPolicy(r.ctx, r.tenantID, domain.DunningPolicy{
				Name: "default", Enabled: true, RetryDays: []int{3, 3, 3},
				FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionNone,
			}); err != nil {
				t.Fatalf("upsert policy: %v", err)
			}

			// The card was declined at t0. A period invoice, as almost every
			// invoice in dunning is, so its retries test grant expiry at its
			// issued_at (t0).
			inv, err := r.invoices.Create(r.ctx, r.tenantID, domain.Invoice{
				CustomerID: cust.ID, InvoiceNumber: "INV-TC-GOODWILL",
				Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed, BillingReason: domain.BillingReasonSubscriptionCycle,
				Currency: "USD", SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
				IssuedAt: &t0,
			})
			if err != nil {
				t.Fatalf("create invoice: %v", err)
			}

			credits := credit.NewService(credit.NewPostgresStore(r.db))
			retrier := &paymentRetrierAdapter{charger: r.charger, invoiceStore: r.invoices, paymentSetups: r.retrier.paymentSetups, credits: credits}
			dunningSvc := dunning.NewService(dstore, retrier, nil)

			// Before the advance the clock reads t0: dunning starts there. The
			// operator grants the goodwill credit at grantedAt.
			atT0 := clock.WithSim(r.ctx, clock.Sim{At: t0, TestClockID: clockID})
			run, err := dunningSvc.StartDunning(atT0, r.tenantID, inv.ID, cust.ID, t0, domain.DunningCausePaymentFailed)
			if err != nil {
				t.Fatalf("start dunning: %v", err)
			}
			if run.NextActionAt == nil || !run.NextActionAt.Equal(firstRetry) {
				t.Fatalf("first retry = %v, want %v", run.NextActionAt, firstRetry)
			}
			expiresAt := tc.expiresAt
			atGrant := clock.WithSim(r.ctx, clock.Sim{At: tc.grantedAt, TestClockID: clockID})
			grant, err := credits.Grant(atGrant, r.tenantID, credit.GrantInput{
				Currency:   "USD",
				CustomerID: cust.ID, AmountCents: 5000, Description: "goodwill", ExpiresAt: &expiresAt,
			})
			if err != nil {
				t.Fatalf("grant credit: %v", err)
			}

			// Every charge Stripe receives is declined, so a charge that
			// should not happen leaves a clean run behind it.
			r.charges.answers = []scriptedCharge{
				{err: stripeDeclined("pi_goodwill_1", "insufficient_funds", "Your card has insufficient funds.")},
				{err: stripeDeclined("pi_goodwill_2", "insufficient_funds", "Your card has insufficient funds.")},
				{err: stripeDeclined("pi_goodwill_3", "insufficient_funds", "Your card has insufficient funds.")},
			}

			clocks := testclock.NewService(testclock.NewPostgresStore(r.db))
			clocks.SetBillingRunner(noBillingRunner{})
			clocks.SetCreditExpirer(credits)
			clocks.SetDunning(dunningSvc)
			if err := clocks.RunCatchup(r.ctx, testclock.CatchupJob{TenantID: r.tenantID, ClockID: clockID, PrevFrozenTime: t0}); err != nil {
				t.Fatalf("catchup: %v", err)
			}

			entries, err := credits.ListEntries(r.ctx, credit.ListFilter{TenantID: r.tenantID, CustomerID: cust.ID, Limit: 100})
			if err != nil {
				t.Fatalf("list ledger: %v", err)
			}
			var usage, expiry []domain.CreditLedgerEntry
			for _, e := range entries {
				switch e.EntryType {
				case domain.CreditUsage:
					usage = append(usage, e)
				case domain.CreditExpiry:
					expiry = append(expiry, e)
				}
			}
			got := r.invoice(inv.ID)
			gotRun, err := dstore.GetRunByInvoice(r.ctx, r.tenantID, inv.ID)
			if err != nil {
				t.Fatalf("get run: %v", err)
			}

			if tc.creditPaysIt {
				if n := len(r.charges.charged); n != 0 {
					t.Fatalf("Stripe received %d charges %v; the grant was live at the invoice's issued_at %v and must pay it at the %v retry",
						n, r.charges.amounts, t0, firstRetry)
				}
				if got.Status != domain.InvoicePaid {
					t.Errorf("invoice %s, want paid", got.Status)
				}
				if gotRun.State != domain.DunningResolved {
					t.Errorf("dunning run %s, want resolved", gotRun.State)
				}
				if len(usage) != 1 || usage[0].AmountCents != -5000 || !usage[0].CreatedAt.Equal(firstRetry) ||
					!drainedOnly(usage[0], grant.ID) {
					t.Errorf("usage entries %+v, want one of -5000 at %v drawn from grant %s", usage, firstRetry, grant.ID)
				}
				if len(expiry) != 0 {
					t.Errorf("expiry entries %+v, want none: the grant was used up before it expired", expiry)
				}
				return
			}

			if len(r.charges.amounts) == 0 || r.charges.amounts[0] != 5000 {
				t.Fatalf("Stripe charges %v, want the %v retry to charge the full 5000: the grant expired before the period ended", r.charges.amounts, firstRetry)
			}
			for _, a := range r.charges.amounts {
				if a != 5000 {
					t.Errorf("Stripe charges %v, want every retry at 5000: a grant expired before the period ended was drawn", r.charges.amounts)
					break
				}
			}
			if got.Status == domain.InvoicePaid {
				t.Error("invoice paid; every charge was declined and the grant had expired")
			}
			if len(usage) != 0 {
				t.Errorf("usage entries %+v, want none: the grant expired before the period ended", usage)
			}
			if len(expiry) != 1 || expiry[0].AmountCents != -5000 || expiry[0].Description != "Expired grant "+grant.ID {
				t.Errorf("expiry entries %+v, want one of -5000 for grant %s", expiry, grant.ID)
			}
		})
	}
}

// drainedOnly reports whether a usage entry drew from exactly one credit
// block, blockID (the drained_blocks attribution in its metadata).
func drainedOnly(e domain.CreditLedgerEntry, blockID string) bool {
	blocks, ok := e.Metadata["drained_blocks"].([]any)
	if !ok || len(blocks) != 1 {
		return false
	}
	b, ok := blocks[0].(map[string]any)
	return ok && b["block_id"] == blockID
}
