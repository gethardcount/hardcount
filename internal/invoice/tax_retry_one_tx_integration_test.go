package invoice

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/subscription"
)

// P1-29: a tax retry writes the tax and finalizes the draft in one
// transaction. When the finalize was a second transaction, a stop between the
// two (a SIGTERM or a lost lease on the tick's ctx, a cut COMMIT, a crash)
// left an engine draft with tax ok, which no sweep lists, so the period was
// never issued. These tests run against real Postgres and the real
// subscription store.

// Pause states for subTaxPendingDraft.
const (
	pauseNone  = ""
	pauseKeep  = "keep_as_draft"
	pauseEnded = "ended" // keep_as_draft with resumes_at an hour ago
)

// subTaxPendingDraft is taxPendingDraft on a real subscription in the given
// pause state.
func (r *snapRig) subTaxPendingDraft(t *testing.T, num, pause string) string {
	t.Helper()
	plan, err := pricing.NewPostgresStore(r.db).CreatePlan(r.ctx, r.tenantID, domain.Plan{
		Code: "g7-plan-" + num, Name: "G7", Currency: "EUR",
		BillingInterval: domain.BillingMonthly, Status: domain.PlanActive, BaseAmountCents: 5000,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	subs := subscription.NewPostgresStore(r.db)
	ps := time.Now().UTC().AddDate(0, -1, 0)
	pe := ps.AddDate(0, 1, 0)
	sub, err := subs.Create(r.ctx, r.tenantID, domain.Subscription{
		Code: "g7-sub-" + num, DisplayName: "G7 Sub", CustomerID: r.custID,
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt: &ps, Items: []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		CurrentBillingPeriodStart: &ps, CurrentBillingPeriodEnd: &pe,
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	if pause != pauseNone {
		pc := domain.PauseCollection{Behavior: domain.PauseCollectionKeepAsDraft}
		if pause == pauseEnded {
			ended := time.Now().UTC().Add(-time.Hour)
			pc.ResumesAt = &ended
		}
		if _, err := subs.SetPauseCollection(r.ctx, r.tenantID, sub.ID, pc, nil); err != nil {
			t.Fatalf("pause collection: %v", err)
		}
	}
	p := r.protoInvoice(num, domain.InvoiceDraft, domain.BillingReasonSubscriptionCycle)
	p.SubscriptionID = sub.ID
	p.TaxStatus = domain.InvoiceTaxPending
	p.TaxErrorCode = "provider_outage"
	d, err := r.store.CreateWithLineItems(r.ctx, r.tenantID, p, nil)
	if err != nil {
		t.Fatalf("create tax-pending draft: %v", err)
	}
	return d.ID
}

// TestRetryTax_PausedSubscription_DraftKept: the pause is read on the tax
// write's transaction. keep_as_draft in force keeps the draft (the tax is
// written, nothing is queued, no error); a pause whose resumes_at has passed
// no longer holds it, as for the sweeps.
//
// Mutation: the hook skips CollectionPausedTx, and the paused case is
// finalized and queued.
func TestRetryTax_PausedSubscription_DraftKept(t *testing.T) {
	cases := []struct {
		name, pause   string
		wantFinalized bool
	}{
		{"running subscription: finalized", pauseNone, true},
		{"keep_as_draft in force: stays a draft", pauseKeep, false},
		{"pause whose resumes_at has passed: finalized", pauseEnded, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newSnapRig(t, "P129 Pause Corp")
			id := r.subTaxPendingDraft(t, "P129-PAUSE", c.pause)
			r.svc.SetTaxRetrier(&resolvingTaxRetrier{db: r.db, store: r.store})

			out, err := r.svc.RetryTax(r.ctx, r.tenantID, id)
			if err != nil {
				t.Fatalf("RetryTax: %v", err)
			}
			got, err := r.store.Get(r.ctx, r.tenantID, id)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got.TaxStatus != domain.InvoiceTaxOK {
				t.Errorf("tax_status = %s, want ok: the tax write is kept", got.TaxStatus)
			}
			if !c.wantFinalized {
				if got.Status != domain.InvoiceDraft || got.AutoChargePending || out.Status != domain.InvoiceDraft {
					t.Errorf("status = %s (returned %s), auto_charge_pending = %v; want a draft, not queued (keep_as_draft)", got.Status, out.Status, got.AutoChargePending)
				}
				return
			}
			if got.Status != domain.InvoiceFinalized || out.Status != domain.InvoiceFinalized {
				t.Fatalf("status = %s (returned %s), want finalized", got.Status, out.Status)
			}
			if !got.AutoChargePending || got.PublicToken == "" {
				t.Errorf("auto_charge_pending = %v, public token %q; want queued with a token", got.AutoChargePending, got.PublicToken)
			}
		})
	}
}

// TestUpdateTaxAtomic_ThenRunsOnTheTransaction: then sees the taxed row on
// the write's transaction, and the row it returns is read after then. A then
// error rolls the tax write back and is returned.
//
// Mutation: commit before then, and the failing hook leaves the tax written.
func TestUpdateTaxAtomic_ThenRunsOnTheTransaction(t *testing.T) {
	r := newSnapRig(t, "P129 Then Corp")

	t.Run("then sees the taxed row; its write is in the returned row", func(t *testing.T) {
		id := r.taxPendingDraft(t, "P129-THEN-1")
		var seen domain.Invoice
		out, err := resolveTaxThen(r.ctx, r.store, r.tenantID, id, func(ctx context.Context, tx *sql.Tx, taxed domain.Invoice) error {
			seen = taxed
			_, err := tx.ExecContext(ctx, `UPDATE invoices SET memo = 'written by then' WHERE id = $1`, taxed.ID)
			return err
		})
		if err != nil {
			t.Fatalf("UpdateTaxAtomic: %v", err)
		}
		if seen.TaxStatus != domain.InvoiceTaxOK {
			t.Errorf("then saw tax_status %s, want ok (the taxed row)", seen.TaxStatus)
		}
		if out.Memo != "written by then" {
			t.Errorf("returned memo = %q, want then's write: the row is read again after then", out.Memo)
		}
	})

	t.Run("a then error rolls the tax write back", func(t *testing.T) {
		id := r.taxPendingDraft(t, "P129-THEN-2")
		before, err := r.store.Get(r.ctx, r.tenantID, id)
		if err != nil {
			t.Fatal(err)
		}
		hookErr := errors.New("finalize failed")
		_, err = resolveTaxThen(r.ctx, r.store, r.tenantID, id, func(context.Context, *sql.Tx, domain.Invoice) error {
			return hookErr
		})
		if !errors.Is(err, hookErr) {
			t.Fatalf("UpdateTaxAtomic error = %v, want then's error", err)
		}
		got, err := r.store.Get(r.ctx, r.tenantID, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.TaxStatus != domain.InvoiceTaxPending || got.TaxErrorCode != before.TaxErrorCode || got.TaxRetryCount != before.TaxRetryCount {
			t.Errorf("tax_status = %s, code = %q, tries = %d; want pending, %q, %d: the write is rolled back",
				got.TaxStatus, got.TaxErrorCode, got.TaxRetryCount, before.TaxErrorCode, before.TaxRetryCount)
		}
	})
}

// failingInTxAudit fails the in-transaction audit write.
type failingInTxAudit struct{}

func (failingInTxAudit) Log(context.Context, string, string, string, string, string, map[string]any) error {
	return nil
}

func (failingInTxAudit) LogInTx(context.Context, *sql.Tx, audit.Entry) error {
	return errors.New("audit insert failed")
}

// TestFinalize_AuditFailureRollsTheFinalizeBack: the finalize audit row is
// written on the finalize's transaction, so a finalize whose row cannot be
// written does not commit. That holds for an operator's Finalize and for a
// tax retry's, which also takes its tax write back with it.
//
// Mutation: the row goes back to a Log after the commit, and the invoice is
// finalized with no row.
func TestFinalize_AuditFailureRollsTheFinalizeBack(t *testing.T) {
	t.Run("operator Finalize", func(t *testing.T) {
		r := newSnapRig(t, "P129 Audit Finalize Corp")
		p := r.protoInvoice("P129-AUD-1", domain.InvoiceDraft, domain.BillingReasonSubscriptionCycle)
		d, err := r.store.CreateWithLineItems(r.ctx, r.tenantID, p, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.svc.SetAuditLogger(failingInTxAudit{})
		if _, err := r.svc.Finalize(r.ctx, r.tenantID, d.ID); err == nil {
			t.Fatal("Finalize = nil with a failing audit write, want the error")
		}
		got, err := r.store.Get(r.ctx, r.tenantID, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != domain.InvoiceDraft || got.PublicToken != "" {
			t.Errorf("status = %s, token %q; want a draft with no token", got.Status, got.PublicToken)
		}
	})

	t.Run("tax retry", func(t *testing.T) {
		r := newSnapRig(t, "P129 Audit Retry Corp")
		id := r.taxPendingDraft(t, "P129-AUD-2")
		r.svc.SetTaxRetrier(&resolvingTaxRetrier{db: r.db, store: r.store})
		r.svc.SetAuditLogger(failingInTxAudit{})
		if _, err := r.svc.RetryTax(r.ctx, r.tenantID, id); err == nil {
			t.Fatal("RetryTax = nil with a failing audit write, want the error")
		}
		got, err := r.store.Get(r.ctx, r.tenantID, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != domain.InvoiceDraft || got.TaxStatus != domain.InvoiceTaxPending {
			t.Errorf("status = %s, tax_status = %s; want draft with tax pending", got.Status, got.TaxStatus)
		}
	})
}
