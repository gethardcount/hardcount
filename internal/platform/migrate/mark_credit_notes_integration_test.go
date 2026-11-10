package migrate

import (
	"context"
	"testing"
	"time"
)

// markCreditNotesVersion is 00224_mark_credit_notes_owing_tax_reversal.sql.
const markCreditNotesVersion = 224

// 00224 marks the credit notes the earlier version's second list arm would
// still have reversed when the upgrade runs, and no others: issued, with an
// amount, no reversal stored, not marked, written within 24 hours, not on a
// test clock, on an invoice Stripe Tax taxed that has tax and a recorded tax
// transaction. It leaves updated_at alone, which is the note's age for the
// refunds-needing-attention rule.
func TestMarkCreditNotesOwingTaxReversal(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_migrate_mark_credit_notes")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, markCreditNotesVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", markCreditNotesVersion, err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	seed := func(query string, args ...any) {
		t.Helper()
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			_ = tx.Rollback()
			t.Fatalf("seed %q: %v", query, err)
		}
	}
	seed(`SET LOCAL app.bypass_rls = 'on'`)
	seed(`SET LOCAL app.livemode = 'off'`)
	seed(`INSERT INTO tenants (id, name) VALUES ('t_mark', 'Mark')`)
	seed(`INSERT INTO customers (id, tenant_id, external_id, display_name) VALUES ('cus_mark', 't_mark', 'ext_mark', 'Mark Co')`)
	invoice := func(id, provider string, taxCents int, taxTx string) {
		seed(`INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, billing_period_start, billing_period_end,
		                            status, tax_provider, tax_amount_cents, tax_transaction_id)
		      VALUES ($1, 't_mark', 'cus_mark', $1, now() - interval '30 days', now(), 'paid', $2, $3, NULLIF($4, ''))`,
			id, provider, taxCents, taxTx)
	}
	invoice("inv_committed", "stripe_tax", 1000, "tx_upstream")
	invoice("inv_not_committed", "stripe_tax", 1000, "")
	invoice("inv_zero_tax", "stripe_tax", 0, "tx_upstream_zero")
	invoice("inv_manual", "manual", 1000, "")

	old := time.Now().UTC().Add(-25 * time.Hour)
	recent := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	note := func(id, invoiceID, status string, totalCents int, reversalTx string, marked, simulated bool, updatedAt time.Time) {
		seed(`INSERT INTO credit_notes (id, tenant_id, invoice_id, customer_id, credit_note_number, reason, status,
		                                total_cents, tax_transaction_id, tax_reversal_pending, is_simulated, updated_at)
		      VALUES ($1, 't_mark', $2, 'cus_mark', $1, 'fraudulent', $3, $4, NULLIF($5, ''), $6, $7, $8)`,
			id, invoiceID, status, totalCents, reversalTx, marked, simulated, updatedAt)
	}
	// The one shape the earlier version's second arm still listed.
	note("cn_owed", "inv_committed", "issued", 5500, "", false, false, recent)
	// Everything else it did not list, or this version does not mark.
	note("cn_older_than_a_day", "inv_committed", "issued", 5500, "", false, false, old)
	note("cn_reversal_stored", "inv_committed", "issued", 5500, "tx_reversal", false, false, recent)
	note("cn_draft", "inv_committed", "draft", 5500, "", false, false, recent)
	note("cn_no_amount", "inv_committed", "issued", 0, "", false, false, recent)
	note("cn_test_clock", "inv_committed", "issued", 5500, "", false, true, recent)
	note("cn_invoice_not_committed", "inv_not_committed", "issued", 5500, "", false, false, recent)
	note("cn_invoice_zero_tax", "inv_zero_tax", "issued", 5500, "", false, false, recent)
	note("cn_invoice_manual", "inv_manual", "issued", 5500, "", false, false, recent)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up through %d: %v", markCreditNotesVersion, err)
	}

	rows, err := db.QueryContext(ctx, `SELECT id, tax_reversal_pending, updated_at FROM credit_notes ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	seen := 0
	for rows.Next() {
		var id string
		var marked bool
		var updatedAt time.Time
		if err := rows.Scan(&id, &marked, &updatedAt); err != nil {
			t.Fatal(err)
		}
		seen++
		if want := id == "cn_owed"; marked != want {
			t.Errorf("%s: tax_reversal_pending = %v, want %v", id, marked, want)
		}
		if id == "cn_owed" && !updatedAt.Equal(recent) {
			t.Errorf("cn_owed: updated_at moved from %s to %s; the migration must not change the note's age", recent, updatedAt.UTC())
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if seen != 9 {
		t.Fatalf("read %d credit notes, want 9 (is row-level security hiding them from this connection?)", seen)
	}
}
