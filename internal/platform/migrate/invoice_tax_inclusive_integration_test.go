package migrate

import (
	"context"
	"database/sql"
	"net/url"
	"testing"
)

// 00227_invoice_tax_inclusive.sql adds the column; 00228 sets it on the
// invoices that existed before.
const (
	invoiceTaxInclusiveVersion         = 227
	invoiceTaxInclusiveBackfillVersion = 228
)

// leastPrivilegeOwner makes the scratch database's owner a role that is not a
// superuser and cannot bypass row-level security, as a managed install sets
// it up (scripts/ci/migrations-fresh-install.sh), and returns its DSN. The
// admin creates the extensions, which need more than the owner has.
func leastPrivilegeOwner(t *testing.T, dbName string) string {
	t.Helper()
	role, password := "owner_"+dbName, "owner-password"
	admin, err := sql.Open("pgx", adminTestURL())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close() }()
	for _, q := range []string{
		`DROP ROLE IF EXISTS ` + quoteIdent(role),
		`CREATE ROLE ` + quoteIdent(role) + ` WITH LOGIN PASSWORD '` + password + `' NOSUPERUSER NOBYPASSRLS CREATEROLE`,
		`ALTER DATABASE ` + quoteIdent(dbName) + ` OWNER TO ` + quoteIdent(role),
	} {
		if _, err := admin.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	scratch, err := sql.Open("pgx", withDBName(t, adminTestURL(), dbName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = scratch.Close() }()
	for _, q := range []string{
		`CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public`,
		`CREATE EXTENSION IF NOT EXISTS citext WITH SCHEMA public`,
		`CREATE EXTENSION IF NOT EXISTS btree_gist WITH SCHEMA public`,
		`ALTER SCHEMA public OWNER TO ` + quoteIdent(role),
	} {
		if _, err := scratch.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	t.Cleanup(func() {
		admin, err := sql.Open("pgx", adminTestURL())
		if err != nil {
			return
		}
		defer func() { _ = admin.Close() }()
		_, _ = admin.Exec(`DROP DATABASE IF EXISTS ` + quoteIdent(dbName) + ` WITH (FORCE)`)
		_, _ = admin.Exec(`DROP ROLE IF EXISTS ` + quoteIdent(role))
	})
	u, err := url.Parse(withDBName(t, adminTestURL(), dbName))
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, password)
	return u.String()
}

// 00228 gives an invoice from before 00227 its tenant's current setting,
// which is how the code reads it today: true only for a tenant whose prices
// include tax (a live and a test-mode invoice), false for one whose prices do
// not, and false for a tenant with no settings row (the setting's default).
// The migrations run as an owner that cannot bypass row-level security, as on
// a managed install: invoices and tenant_settings force it, so without its
// own bypass the backfill would match no row and say nothing. The Down
// removes the column, and Up runs again.
func TestInvoiceTaxInclusive_BackfillFromTheTenantsSetting(t *testing.T) {
	const name = "hardcount_migrate_invoice_tax_inclusive"
	_, db := scratchDB(t, name)
	ownerDSN := leastPrivilegeOwner(t, name)
	ctx := context.Background()
	if err := up(ctx, ownerDSN, filesBefore(t, invoiceTaxInclusiveVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", invoiceTaxInclusiveVersion, err)
	}
	exec := func(livemode, q string) {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`SELECT set_config('app.bypass_rls', 'on', true), set_config('app.livemode', $1, true)`, livemode); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(q); err != nil {
			t.Fatalf("%s: %v", oneLine(q), err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	invoices := []struct {
		id, tenant, livemode string
		want                 bool
	}{
		{"inv_inc_live", "ten_inc", "on", true},
		{"inv_inc_test", "ten_inc", "off", true},
		{"inv_exc", "ten_exc", "on", false},
		{"inv_none", "ten_none", "on", false},
	}
	for _, tn := range []struct{ id, settings string }{{"ten_inc", "true"}, {"ten_exc", "false"}, {"ten_none", ""}} {
		exec("on", `INSERT INTO tenants (id, name) VALUES ('`+tn.id+`', '`+tn.id+`')`)
		if tn.settings != "" {
			exec("on", `INSERT INTO tenant_settings (tenant_id, tax_inclusive) VALUES ('`+tn.id+`', `+tn.settings+`)`)
		}
		exec("on", `INSERT INTO customers (id, tenant_id, external_id, display_name) VALUES ('cus_`+tn.id+`', '`+tn.id+`', 'c', 'C')`)
	}
	for _, inv := range invoices {
		exec(inv.livemode, `INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, status, payment_status, currency,
				subtotal_cents, total_amount_cents, amount_due_cents, amount_paid_cents, billing_period_start, billing_period_end)
			VALUES ('`+inv.id+`', '`+inv.tenant+`', 'cus_`+inv.tenant+`', 'TI-`+inv.id+`', 'finalized', 'pending', 'USD',
				12000, 12000, 12000, 0, '2026-10-01', '2026-11-01')`)
	}

	read := func(id string) bool {
		t.Helper()
		var got bool
		if err := db.QueryRow(`SELECT tax_inclusive FROM invoices WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	// 00227 alone: every invoice has the column's default.
	if err := up(ctx, ownerDSN, filesBefore(t, invoiceTaxInclusiveBackfillVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", invoiceTaxInclusiveBackfillVersion, err)
	}
	for _, inv := range invoices {
		if read(inv.id) {
			t.Errorf("%s: tax_inclusive true before the backfill, want the default false", inv.id)
		}
	}
	if err := up(ctx, ownerDSN, migrationFiles()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	for _, inv := range invoices {
		if got := read(inv.id); got != inv.want {
			t.Errorf("%s (%s, livemode %s): tax_inclusive %v after the backfill, want %v", inv.id, inv.tenant, inv.livemode, got, inv.want)
		}
	}

	if _, err := down(ctx, ownerDSN, migrationFiles(), invoiceTaxInclusiveVersion-1); err != nil {
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
	if err := up(ctx, ownerDSN, migrationFiles()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
}
