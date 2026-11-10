package billing_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/tax"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// The lock order of a cancel against a credit note, on real Postgres (ADR-118
// amendment 2026-11-09). Two locks are held to commit:
//   - the tenant's number row (tenant_settings), which numbers every invoice
//     and every credit note;
//   - an invoice's credit-note lock, an advisory lock every credit-note
//     writer takes before it numbers its note.
//
// A cancel takes its relief's credit-note locks before the number row, as
// every credit-note writer does. A credit-note create takes its invoice's row
// (FOR KEY SHARE) before the number row, so a relief that voided that invoice
// waits for the create instead of deadlocking with it.
//
// Each pair test holds the number row in a gate transaction, starts both
// writers so that each one waits, then releases the gate. In the wrong order
// Postgres aborts one of them with 40P01 (deadlock detected).

const lockOrderNoteCents = 100

// discardOutbox stands in for the webhook outbox of a recorded Stripe refund.
type discardOutbox struct{}

func (discardOutbox) Enqueue(context.Context, *sql.Tx, string, string, map[string]any) (string, error) {
	return "evt_test", nil
}

// holdNumberRow locks the tenant's number row FOR UPDATE in a transaction it
// returns open. Rolling it back lets the writers waiting behind it go, one at
// a time, in the order they arrived: each one queues for the row's tuple lock.
// The gate must not update the row. Writers would then wait for the gate's
// transaction instead, and race for the row once it ends.
func holdNumberRow(t *testing.T, ctx context.Context, db *postgres.DB, tenantID string) *sql.Tx {
	t.Helper()
	gate, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin gate: %v", err)
	}
	t.Cleanup(func() { postgres.Rollback(gate) })
	var one int
	if err := gate.QueryRowContext(ctx, `SELECT 1 FROM tenant_settings WHERE tenant_id = $1 FOR UPDATE`, tenantID).Scan(&one); err != nil {
		t.Fatalf("gate: lock the number row: %v", err)
	}
	return gate
}

// requireBothCommitted fails the test unless every writer returned nil, and
// says which one Postgres aborted as a deadlock victim.
func requireBothCommitted(t *testing.T, results map[string]error) {
	t.Helper()
	for name, err := range results {
		switch {
		case postgres.IsDeadlock(err):
			t.Errorf("%s was aborted as a deadlock victim (40P01): %v", name, err)
		case err != nil:
			t.Errorf("%s: %v", name, err)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
}

// creditNotesOn lists the credit notes on an invoice.
func creditNotesOn(t *testing.T, ctx context.Context, db *postgres.DB, tenantID, invoiceID string) []domain.CreditNote {
	t.Helper()
	notes, err := creditnote.NewPostgresStore(db).List(ctx, creditnote.ListFilter{TenantID: tenantID, InvoiceID: invoiceID})
	if err != nil {
		t.Fatalf("list credit notes on %s: %v", invoiceID, err)
	}
	return notes
}

// operatorCreditNote is the operator's credit note of 100c on invoiceID.
func operatorCreditNote(ctx context.Context, cn *creditnote.Service, tenantID, invoiceID string) (string, error) {
	note, err := cn.Create(ctx, tenantID, creditnote.CreateInput{
		InvoiceID: invoiceID, Reason: "goodwill",
		Lines: []creditnote.CreditLineInput{{Description: "goodwill", Quantity: 1, UnitAmountCents: lockOrderNoteCents}},
	})
	return note.ID, err
}

// A cancel that bills a final invoice and credits the paid day-1 invoice,
// against a credit note on day-1 at the same moment: the operator's, or one
// recording a refund made in Stripe. The cancel bills 3000c of usage (it
// numbers an invoice) and writes a 2450c relief draft on day-1 (it takes
// day-1's credit-note lock and numbers a note).
//
// Mutations: in writePlanned, write the relief after the final invoice, or
// number the final invoice before the relief → all four
// cases abort one writer with 40P01.
func TestCancel_ConcurrentCreditNoteOnFundingInvoice_BothCommit(t *testing.T) {
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)

	cancels := []struct {
		name string
		// prepare does what comes before the cancel and returns the cancel.
		prepare func(*reliefCancelRig, *testing.T) func() error
	}{
		{"immediate cancel", func(r *reliefCancelRig, _ *testing.T) func() error {
			return func() error { _, _, err := r.cancel(); return err }
		}},
		{"scheduled cancel", func(r *reliefCancelRig, t *testing.T) func() error {
			r.scheduleCancel(t)
			return r.fire
		}},
	}
	writers := []struct {
		name string
		// prepare wires what the writer needs and returns it. The writer
		// returns the id of the credit note it made.
		prepare func(*reliefCancelRig) func() (string, error)
	}{
		{"operator credit note", func(r *reliefCancelRig) func() (string, error) {
			return func() (string, error) { return operatorCreditNote(r.ctx, r.cn, r.tenantID, r.day1ID) }
		}},
		{"Stripe refund recorded", func(r *reliefCancelRig) func() (string, error) {
			r.cn.SetStripeRefundRecording(creditnote.NewPostgresStore(r.db), r.invStore, discardOutbox{})
			return func() (string, error) {
				res, err := r.cn.RecordStripeRefund(r.ctx, r.tenantID, domain.StripeRefundSnapshot{
					StripeEventID: "evt_lock_order", EventAt: r.cancelAt, StripeRefundID: "re_lock_order",
					StripePaymentIntentID: "pi_day1", AmountCents: lockOrderNoteCents, Currency: "usd",
					Status: domain.StripeRefundSucceeded, CreatedAt: r.cancelAt,
				})
				return res.CreditNoteID, err
			}
		}},
	}

	for _, c := range cancels {
		for _, w := range writers {
			t.Run(c.name+"/"+w.name, func(t *testing.T) {
				const usageCents = 3000
				r := newReliefCancelRig(t, db, "P1-08 "+c.name+" "+w.name, reliefRigOpts{usageUnits: usageCents})
				cancel, write := c.prepare(r, t), w.prepare(r)

				gate := holdNumberRow(t, r.ctx, r.db, r.tenantID)
				cancelErr := make(chan error, 1)
				go func() { cancelErr <- cancel() }()
				testutil.WaitForLockWaiter(t, r.ctx, admin, "tenant_settings")

				var noteID string
				writeErr := make(chan error, 1)
				go func() {
					id, err := write()
					noteID = id
					writeErr <- err
				}()
				testutil.WaitForLockWaiters(t, r.ctx, admin, "", 2)
				postgres.Rollback(gate)

				requireBothCommitted(t, map[string]error{"the cancel": <-cancelErr, "the " + w.name: <-writeErr})

				if noteID == "" {
					t.Fatalf("the %s made no credit note: it never asked for the number row, so this case proves nothing", w.name)
				}
				sub, err := subscription.NewPostgresStore(db).Get(r.ctx, r.tenantID, r.subID)
				if err != nil || sub.Status != domain.SubscriptionCanceled {
					t.Fatalf("subscription: %v status=%s, want canceled", err, sub.Status)
				}
				final := r.finalInvoice(t)
				if final.TotalAmountCents != usageCents || final.CreditsAppliedCents != reliefCents {
					t.Errorf("final invoice total=%dc credits_applied=%dc, want %dc reduced by the %dc relief",
						final.TotalAmountCents, final.CreditsAppliedCents, usageCents, reliefCents)
				}
				var totals []int64
				for _, n := range creditNotesOn(t, r.ctx, db, r.tenantID, r.day1ID) {
					totals = append(totals, n.TotalCents)
				}
				slices.Sort(totals)
				if !slices.Equal(totals, []int64{lockOrderNoteCents, reliefCents}) {
					t.Errorf("credit notes on day-1 = %v, want the %dc note and the %dc relief", totals, lockOrderNoteCents, reliefCents)
				}
			})
		}
	}
}

// probeTaxResolver serves the tenant's real tax provider, wrapped so that
// each Calculate first runs probe.
type probeTaxResolver struct {
	inner *tax.Resolver
	probe func()
}

func (p *probeTaxResolver) Resolve(ctx context.Context, ts domain.TenantSettings) (tax.Provider, error) {
	prov, err := p.inner.Resolve(ctx, ts)
	if err != nil {
		return nil, err
	}
	return probedProvider{Provider: prov, probe: p.probe}, nil
}

type probedProvider struct {
	tax.Provider
	probe func()
}

func (p probedProvider) Calculate(ctx context.Context, req tax.Request) (*tax.Result, error) {
	p.probe()
	return p.Provider.Calculate(ctx, req)
}

// The final invoice's tax call is a network call for a Stripe Tax tenant. It
// runs before the cancel takes the number row or a credit-note lock, so no
// invoice or credit note of the tenant waits on it.
//
// Mutation: price the final invoice in writePlanned, after writing the
// relief → the tax call runs holding both locks.
func TestCancel_TaxCallHoldsNoNumberRowOrCreditNoteLock(t *testing.T) {
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	r := newReliefCancelRig(t, db, "P1-08 tax call holds no lock", reliefRigOpts{usageUnits: 3000})

	type probeResult struct{ numberRowFree, creditNoteLockFree bool }
	var (
		mu     sync.Mutex
		probes []probeResult
	)
	probe := func() {
		var got probeResult
		tx, err := admin.BeginTx(r.ctx, nil)
		if err != nil {
			t.Errorf("probe: begin: %v", err)
			return
		}
		_, err = tx.ExecContext(r.ctx, `SELECT 1 FROM tenant_settings WHERE tenant_id = $1 FOR UPDATE NOWAIT`, r.tenantID)
		got.numberRowFree = err == nil
		_ = tx.Rollback() // the probe only asked for the lock; nothing to keep
		if err := admin.QueryRowContext(r.ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))`,
			r.tenantID+":"+r.day1ID).Scan(&got.creditNoteLockFree); err != nil {
			t.Errorf("probe: try the credit-note lock: %v", err)
		}
		mu.Lock()
		probes = append(probes, got)
		mu.Unlock()
	}
	r.engine.SetTaxProviderResolver(&probeTaxResolver{inner: tax.NewResolver(nil), probe: probe})

	r.cancelImmediately(t)

	mu.Lock()
	defer mu.Unlock()
	if len(probes) != 1 {
		t.Fatalf("tax calculations during the cancel = %d, want 1 (the final invoice's)", len(probes))
	}
	if p := probes[0]; !p.numberRowFree || !p.creditNoteLockFree {
		t.Errorf("during the final invoice's tax call: number row free=%v, day-1 credit-note lock free=%v; want both free",
			p.numberRowFree, p.creditNoteLockFree)
	}
	if r.finalInvoice(t).TotalAmountCents != 3000 {
		t.Error("the final invoice is not the 3000c of usage")
	}
}

// A relief that voids an unused invoice V, against an operator credit note on
// V at the same moment. The relief locks V's row FOR UPDATE and then takes the
// number row: for the cancel's final invoice, or the swap's new invoice. The
// credit note takes V's row before the number row, so the void waits for it.
//
// Mutation: drop the FOR KEY SHARE read from creditnote's
// createUnderInvoiceLockInTx → the note waits for V's row at its insert while
// holding the number row, and both cases abort one writer with 40P01.
func TestReliefVoid_ConcurrentCreditNoteOnTheVoidedInvoice_BothCommit(t *testing.T) {
	// run holds the number row, starts the credit note on voidID, waits for it
	// to wait, starts change, waits for both, and releases the gate.
	run := func(t *testing.T, ctx context.Context, db *postgres.DB, cn *creditnote.Service, tenantID, voidID string, change func() error) {
		t.Helper()
		admin := testutil.AdminPool(t)
		gate := holdNumberRow(t, ctx, db, tenantID)
		noteErr := make(chan error, 1)
		go func() {
			_, err := operatorCreditNote(ctx, cn, tenantID, voidID)
			noteErr <- err
		}()
		testutil.WaitForLockWaiter(t, ctx, admin, "tenant_settings")
		changeErr := make(chan error, 1)
		go func() { changeErr <- change() }()
		testutil.WaitForLockWaiters(t, ctx, admin, "", 2)
		postgres.Rollback(gate)

		requireBothCommitted(t, map[string]error{"the credit note": <-noteErr, "the change": <-changeErr})

		v, err := invoice.NewPostgresStore(db).Get(ctx, tenantID, voidID)
		if err != nil || v.Status != domain.InvoiceVoided {
			t.Errorf("invoice %s: %v status=%s, want voided", voidID, err, v.Status)
		}
		notes := creditNotesOn(t, ctx, db, tenantID, voidID)
		if len(notes) != 1 || notes[0].TotalCents != lockOrderNoteCents || notes[0].Status != domain.CreditNoteDraft {
			t.Errorf("credit notes on the voided invoice = %+v, want one %dc draft", notes, lockOrderNoteCents)
		}
	}

	t.Run("immediate cancel", func(t *testing.T) {
		const usageCents = 3000
		db := testutil.SetupTestDB(t)
		r := newReliefCancelRig(t, db, "P1-08 relief void vs credit note: cancel", reliefRigOpts{usageUnits: usageCents, sameDay: true})
		run(t, r.ctx, db, r.cn, r.tenantID, r.day1ID, func() error { _, _, err := r.cancel(); return err })

		sub, err := subscription.NewPostgresStore(db).Get(r.ctx, r.tenantID, r.subID)
		if err != nil || sub.Status != domain.SubscriptionCanceled {
			t.Errorf("subscription: %v status=%s, want canceled", err, sub.Status)
		}
		if final := r.finalInvoice(t); final.TotalAmountCents != usageCents {
			t.Errorf("final invoice total = %dc, want %dc", final.TotalAmountCents, usageCents)
		}
	})

	t.Run("same-day cross-interval plan swap", func(t *testing.T) {
		r := newPrebillVoidRig(t, "P1-08 relief void vs credit note: swap", waitingPolicy())
		run(t, r.ctx, r.db, r.cn, r.tenantID, r.day1.ID, func() error {
			tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
			if err != nil {
				return err
			}
			defer postgres.Rollback(tx)
			res, err := r.subSvc.UpdateItemTx(r.ctx, tx, r.tenantID, r.sub.ID, r.sub.Items[0].ID, subscription.UpdateItemInput{
				NewPlanID: r.yearly.ID, Immediate: true,
			})
			if err != nil {
				return err
			}
			if !res.OrchestratedCrossAxis {
				return errors.New("the monthly-to-yearly swap did not take the atomic cross-interval path")
			}
			return tx.Commit()
		})
	})
}
