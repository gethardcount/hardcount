package api

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// An operator waives a dunned invoice with a credit note for what is due (G3,
// loops review 2026-10-19). The note marks the invoice paid, and its dunning
// run closes as payment_recovered at once, on the server as router.go wires it.
//
// The escalated run is the case that matters: under the default policy (no
// final action) every run whose retries ran out ends escalated with its
// invoice still open, which is when an operator is most likely to waive it.
// The dunning ticks skip escalated runs, so before, the run stayed escalated
// for good. An active run closed only at its next retry, days later.
//
// Mutation seam: remove the ResolveByInvoice call after Issue commits, or the
// SetDunningResolver line in router.go → both subtests go red.
func TestCreditNoteToZero_ResolvesEscalatedRun(t *testing.T) {
	t.Run("escalated run", func(t *testing.T) {
		r := newZeroDueDunningRig(t, "CN Zero Due Escalated")
		run := r.run(r.policy.MaxRetryAttempts, time.Now().UTC().Add(-time.Minute))
		if _, errs := r.srv.DunningSvc.ProcessDueRuns(r.ctx, r.tenantID, 20); len(errs) > 0 {
			t.Fatalf("dunning tick: %v", errs)
		}
		if got := r.getRun(run.ID); got.State != domain.DunningEscalated {
			t.Fatalf("run %s after its retries ran out, want escalated", got.State)
		}

		r.waive()
		r.assertRecovered(run.ID)
	})

	t.Run("active run", func(t *testing.T) {
		r := newZeroDueDunningRig(t, "CN Zero Due Active")
		run := r.run(1, time.Now().UTC().Add(96*time.Hour)) // next retry in four days

		r.waive()
		r.assertRecovered(run.ID)
	})
}

type zeroDueDunningRig struct {
	t        *testing.T
	ctx      context.Context
	db       *postgres.DB
	srv      *Server
	tenantID string
	invoices *invoice.PostgresStore
	runs     *dunning.PostgresStore
	policy   domain.DunningPolicy
	cust     domain.Customer
	inv      domain.Invoice
}

// newZeroDueDunningRig is the production server over real Postgres, a tenant
// on the default dunning policy, and a finalized 50.00 invoice whose card
// payment failed.
func newZeroDueDunningRig(t *testing.T, name string) *zeroDueDunningRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	t.Cleanup(cancel)
	ctx = leadertest.Token(t, testutil.AdminPool(t), ctx, leader.RoleDunning)
	r := &zeroDueDunningRig{t: t, ctx: ctx, db: db, srv: NewServer(db, clock.Real(), nil),
		tenantID: testutil.CreateTestTenant(t, db, name), invoices: invoice.NewPostgresStore(db), runs: dunning.NewPostgresStore(db)}

	policy, err := r.srv.DunningSvc.UpsertPolicy(ctx, r.tenantID, dunning.DefaultSeedPolicy())
	if err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	r.policy = policy
	r.cust, err = customer.NewPostgresStore(db).Create(ctx, r.tenantID, domain.Customer{
		ExternalID: "ext_waived", DisplayName: "Waived", Email: "ap@waived.test",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	now := time.Now().UTC()
	r.inv, err = r.invoices.Create(ctx, r.tenantID, domain.Invoice{
		CustomerID: r.cust.ID, InvoiceNumber: "INV-G3-WAIVED", Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed,
		Currency: "USD", SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
		BillingPeriodStart: now.Add(-30 * 24 * time.Hour), BillingPeriodEnd: now, IssuedAt: &now,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	return r
}

// run is the invoice's dunning run with `attempts` retries used, the next one
// at next.
func (r *zeroDueDunningRig) run(attempts int, next time.Time) domain.InvoiceDunningRun {
	r.t.Helper()
	run, err := r.runs.CreateRun(r.ctx, r.tenantID, domain.InvoiceDunningRun{
		InvoiceID: r.inv.ID, CustomerID: r.cust.ID, PolicyID: r.policy.ID,
		State: domain.DunningActive, Reason: "payment_failed", AttemptCount: attempts, NextActionAt: &next,
	})
	if err != nil {
		r.t.Fatalf("create run: %v", err)
	}
	if ok, err := r.runs.UpdateRunIfActive(r.ctx, r.tenantID, run, attempts, nil); err != nil || !ok {
		r.t.Fatalf("record the attempts: applied=%v err=%v", ok, err)
	}
	return run
}

func (r *zeroDueDunningRig) getRun(id string) domain.InvoiceDunningRun {
	r.t.Helper()
	run, err := r.runs.GetRun(r.ctx, r.tenantID, id)
	if err != nil {
		r.t.Fatalf("get run: %v", err)
	}
	return run
}

// waive issues a credit note for everything due, as the dashboard does: create,
// then issue.
func (r *zeroDueDunningRig) waive() {
	r.t.Helper()
	cn, err := r.srv.CreditNoteSvc.Create(r.ctx, r.tenantID, creditnote.CreateInput{
		InvoiceID: r.inv.ID, Reason: "waived",
		Lines: []creditnote.CreditLineInput{{Description: "waive the invoice", Quantity: 1, UnitAmountCents: r.inv.AmountDueCents}},
	})
	if err != nil {
		r.t.Fatalf("create credit note: %v", err)
	}
	if _, err := r.srv.CreditNoteSvc.Issue(r.ctx, r.tenantID, cn.ID); err != nil {
		r.t.Fatalf("issue credit note: %v", err)
	}
}

// assertRecovered checks the invoice is paid and the run closed once, as
// payment_recovered, with one dunning.resolved queued.
func (r *zeroDueDunningRig) assertRecovered(runID string) {
	r.t.Helper()
	inv, err := r.invoices.Get(r.ctx, r.tenantID, r.inv.ID)
	if err != nil {
		r.t.Fatalf("get invoice: %v", err)
	}
	if inv.Status != domain.InvoicePaid {
		r.t.Errorf("invoice %s/%s with %d due, want paid", inv.Status, inv.PaymentStatus, inv.AmountDueCents)
	}
	if got := r.getRun(runID); got.State != domain.DunningResolved || got.Resolution != domain.ResolutionPaymentRecovered {
		r.t.Errorf("run %s/%q right after the credit note, want resolved as payment_recovered", got.State, got.Resolution)
	}
	tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(r.ctx, `SELECT count(*) FROM webhook_outbox WHERE tenant_id = $1 AND event_type = $2`,
		r.tenantID, domain.EventDunningResolved).Scan(&n); err != nil {
		r.t.Fatalf("count dunning.resolved: %v", err)
	}
	if n != 1 {
		r.t.Errorf("%d dunning.resolved events queued, want exactly 1", n)
	}
}
