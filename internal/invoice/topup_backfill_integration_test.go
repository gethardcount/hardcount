package invoice_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// topupBackfill is the UPDATE of the migration that records refunds and
// losses on a top-up, which makes an old paid prepaid-credit invoice a
// top-up; "" when the migration has none.
func topupBackfill(t *testing.T) string {
	t.Helper()
	paths, err := filepath.Glob("../platform/migrate/sql/*_dispute_loss_credit_note.sql")
	if err != nil || len(paths) != 1 {
		t.Fatalf("the migration: %v, %v", paths, err)
	}
	src, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	up := strings.SplitN(string(src), "-- +goose Down", 2)[0]
	start := strings.Index(up, "UPDATE invoices i SET billing_reason = 'topup'")
	if start < 0 {
		return ""
	}
	end := strings.Index(up[start:], ";")
	return up[start : start+end+1]
}

// TestTopupBackfill_OldPaidCommitInvoices: an upgrade from v0.10.0 makes a
// paid invoice that sold prepaid credit and nothing else, untaxed and with
// its commit grant, a top-up, so relief takes its credit back. Each other
// old shape stays as it was: taxed, with no grant, with a second line,
// finalized and not paid, voided. The rows are built as v0.10.0 left them
// (billing_reason 'manual' or NULL), and the migration's own UPDATE runs on
// them, scoped to the test's account.
func TestTopupBackfill_OldPaidCommitInvoices(t *testing.T) {
	h, ctx := newCommitHarness(t, "Topup Backfill")
	cnSvc := newCardReliefService(h, &stripeRefunds{})
	// inTenant runs q in the test's account and mode, as the app does.
	inTenant := func(t *testing.T, q string, args ...any) {
		t.Helper()
		tx, err := h.db.BeginTx(ctx, postgres.TxTenant, h.tenantID)
		if err != nil {
			t.Fatal(err)
		}
		defer postgres.Rollback(tx)
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	// old is a paid prepaid-credit invoice with its grant, as v0.10.0 left
	// it, then changed by set.
	old := func(t *testing.T, reason string, set string) string {
		t.Helper()
		n++
		inv := h.createTopup(t, ctx, "USD", 9000, 10000)
		payOffline(t, h, ctx, inv.ID)
		inTenant(t, `UPDATE invoices SET billing_reason = NULLIF($2, '') WHERE id = $1`, inv.ID, reason)
		if set != "" {
			inTenant(t, set, inv.ID, fmt.Sprintf("old_%d", n))
		}
		return inv.ID
	}
	const (
		taxed    = `UPDATE invoices SET tax_amount_cents = 700 WHERE id = $1 AND $2 <> ''`
		unpaid   = `UPDATE invoices SET status = 'finalized', payment_status = 'pending', paid_at = NULL, amount_paid_cents = 0, amount_due_cents = total_amount_cents WHERE id = $1 AND $2 <> ''`
		voided   = `UPDATE invoices SET status = 'voided', payment_status = 'pending', paid_at = NULL, amount_paid_cents = 0, voided_at = now() WHERE id = $1 AND $2 <> ''`
		noGrant  = `UPDATE customer_credit_ledger SET source_invoice_id = NULL WHERE source_invoice_id = $1 AND grant_kind = 'commit' AND $2 <> ''`
		twoLines = `INSERT INTO invoice_line_items (id, tenant_id, invoice_id, line_type, description, amount_cents, total_amount_cents)
			SELECT 'hc_li_' || $2, tenant_id, id, 'add_on', 'Setup', 0, 0 FROM invoices WHERE id = $1`
	)
	rows := []struct {
		name, id, want string
	}{
		{"a plain one", old(t, "manual", ""), "topup"},
		{"a plain one with no billing reason", old(t, "", ""), "topup"},
		{"a taxed one", old(t, "manual", taxed), "manual"},
		{"one with no grant", old(t, "manual", noGrant), "manual"},
		{"one with a second line", old(t, "manual", twoLines), "manual"},
		{"a finalized one, not paid", old(t, "manual", unpaid), "manual"},
		{"a voided one", old(t, "manual", voided), "manual"},
	}
	plain := rows[0].id
	relief := func() (domain.CreditNote, error) {
		return cnSvc.CreateAndIssueCommitRelief(ctx, h.tenantID, creditnote.CommitReliefInput{InvoiceID: plain, RetireCents: 6000})
	}
	if _, err := relief(); !errors.Is(err, errs.ErrValidation) || errs.Field(err) != "invoice_id" {
		t.Fatalf("relief before the upgrade: %v, want 422 on invoice_id: it is no top-up yet", err)
	}

	backfill := topupBackfill(t)
	if backfill == "" {
		t.Error("the migration has no backfill of old paid prepaid-credit invoices")
	} else {
		inTenant(t, backfill)
	}

	for _, row := range rows {
		inv, err := h.invStore.Get(ctx, h.tenantID, row.id)
		if err != nil {
			t.Fatal(err)
		}
		got := inv.BillingReason
		if got == "" {
			got = "manual"
		}
		if row.want == "topup" && got != domain.BillingReasonTopup || row.want == "manual" && got == domain.BillingReasonTopup {
			t.Errorf("%s: billing_reason %q, want %s", row.name, inv.BillingReason, row.want)
		}
	}
	// As a top-up, relief takes 60.00 of credit back for its price, 54.00.
	cn, err := relief()
	if err != nil || cn.CommitRetiredCents != 6000 || cn.TotalCents != 5400 || cn.OutOfBandAmountCents != 5400 {
		t.Errorf("relief after the upgrade: %+v, %v; want 54.00 out of band for 60.00 of credit", cn, err)
	}
}
