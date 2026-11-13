package migrate

import (
	"context"
	"testing"
)

// invoiceTaxInclusiveVersion is 00227_invoice_tax_inclusive.sql.
const invoiceTaxInclusiveVersion = 227

// 00227 adds invoices.tax_inclusive. An invoice from before takes its
// tenant's current setting, which is how the code reads it today: true only
// for a tenant whose prices include tax, false for one whose prices do not,
// and false for a tenant with no settings row (the setting's default). The
// Down removes the column, and Up runs again.
func TestInvoiceTaxInclusive_BackfillFromTheTenantsSetting(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_migrate_invoice_tax_inclusive")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, invoiceTaxInclusiveVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", invoiceTaxInclusiveVersion, err)
	}
	exec := func(q string) {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`SELECT set_config('app.bypass_rls', 'on', true), set_config('app.livemode', 'on', true)`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(q); err != nil {
			t.Fatalf("%s: %v", oneLine(q), err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	tenants := []struct {
		id       string
		settings string // the tenant_settings row, or "" for none
		want     bool
	}{
		{"ten_inc", "true", true},
		{"ten_exc", "false", false},
		{"ten_none", "", false},
	}
	for _, tn := range tenants {
		exec(`INSERT INTO tenants (id, name) VALUES ('` + tn.id + `', '` + tn.id + `')`)
		if tn.settings != "" {
			exec(`INSERT INTO tenant_settings (tenant_id, tax_inclusive) VALUES ('` + tn.id + `', ` + tn.settings + `)`)
		}
		exec(`INSERT INTO customers (id, tenant_id, external_id, display_name) VALUES ('cus_` + tn.id + `', '` + tn.id + `', 'c', 'C')`)
		exec(`INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, status, payment_status, currency,
				subtotal_cents, total_amount_cents, amount_due_cents, amount_paid_cents, billing_period_start, billing_period_end)
			VALUES ('inv_` + tn.id + `', '` + tn.id + `', 'cus_` + tn.id + `', 'TI-` + tn.id + `', 'finalized', 'pending', 'USD',
				12000, 12000, 12000, 0, '2026-10-01', '2026-11-01')`)
	}

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	for _, tn := range tenants {
		var got bool
		if err := db.QueryRow(`SELECT tax_inclusive FROM invoices WHERE id = 'inv_` + tn.id + `'`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != tn.want {
			t.Errorf("%s: an invoice from before the migration has tax_inclusive %v, want %v", tn.id, got, tn.want)
		}
	}

	if _, err := down(ctx, dsn, migrationFiles(), invoiceTaxInclusiveVersion-1); err != nil {
		t.Fatalf("Down to before %d: %v", invoiceTaxInclusiveVersion, err)
	}
	var exists bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_name = 'invoices' AND column_name = 'tax_inclusive')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("the Down left invoices.tax_inclusive")
	}
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
}
