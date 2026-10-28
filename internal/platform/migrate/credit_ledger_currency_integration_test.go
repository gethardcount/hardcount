package migrate

import (
	"context"
	"testing"
)

// creditLedgerCurrencyVersion is 00199_credit_ledger_currency.sql; 00200
// validates its CHECK and sets NOT NULL.
const creditLedgerCurrencyVersion = 199

// T12 (ADR-129). 00199 gives every existing ledger row its customer's
// currency in the order a balance used to be read in (billing profile, live
// plan, newest invoice, workspace default), uppercased; 00200 validates the
// CHECK and makes the column NOT NULL; NULL, lowercase and short codes are
// refused afterwards; Down then Up works.
func TestCreditLedgerCurrency_BackfillValidateAndRollback(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_test_ccyb_b_migrate")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, creditLedgerCurrencyVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", creditLedgerCurrencyVersion, err)
	}
	execErr := func(q string) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`SELECT set_config('app.bypass_rls', 'on', true), set_config('app.livemode', 'on', true)`); err != nil {
			return err
		}
		if _, err := tx.Exec(q); err != nil {
			return err
		}
		return tx.Commit()
	}
	exec := func(q string) {
		t.Helper()
		if err := execErr(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO tenants (id, name) VALUES ('ten_ccy', 'Ccy')`)
	exec(`INSERT INTO tenant_settings (tenant_id, default_currency) VALUES ('ten_ccy', 'GBP')`)
	exec(`INSERT INTO customers (id, tenant_id, external_id, display_name) VALUES
		('cus_profile', 'ten_ccy', 'p', 'P'), ('cus_plan', 'ten_ccy', 'l', 'L'), ('cus_none', 'ten_ccy', 'n', 'N')`)
	exec(`INSERT INTO customer_billing_profiles (customer_id, tenant_id, currency) VALUES ('cus_profile', 'ten_ccy', 'EUR')`)
	exec(`INSERT INTO plans (id, tenant_id, code, name, currency, billing_interval, status) VALUES ('pln_usd', 'ten_ccy', 'p', 'P', 'usd', 'monthly', 'active')`)
	exec(`INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status) VALUES ('sub_l', 'ten_ccy', 'l', 'L', 'cus_plan', 'active')`)
	exec(`INSERT INTO subscription_items (id, tenant_id, subscription_id, plan_id, quantity) VALUES ('si_l', 'ten_ccy', 'sub_l', 'pln_usd', 1)`)
	for _, c := range []string{"cus_profile", "cus_plan", "cus_none"} {
		exec(`INSERT INTO customer_credit_ledger (id, tenant_id, customer_id, entry_type, amount_cents, balance_after, description) VALUES
			('ccl_g_` + c + `', 'ten_ccy', '` + c + `', 'grant', 1000, 1000, 'g'),
			('ccl_a_` + c + `', 'ten_ccy', '` + c + `', 'adjustment', -200, 800, 'a')`)
	}

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	want := map[string]string{"cus_profile": "EUR", "cus_plan": "USD", "cus_none": "GBP"}
	rows, err := db.Query(`SELECT customer_id, currency FROM customer_credit_ledger`)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		var cust string
		var cur *string
		if err := rows.Scan(&cust, &cur); err != nil {
			t.Fatal(err)
		}
		n++
		if cur == nil || *cur != want[cust] {
			t.Errorf("customer %s row currency %v, want %s", cust, cur, want[cust])
		}
	}
	_ = rows.Close()
	if n != 6 {
		t.Errorf("ledger rows after Up = %d, want 6", n)
	}
	var validated, notNull bool
	if err := db.QueryRow(`SELECT convalidated FROM pg_constraint WHERE conname = 'customer_credit_ledger_currency_check'`).Scan(&validated); err != nil {
		t.Fatalf("read currency CHECK: %v", err)
	}
	if !validated {
		t.Error("00200 left the currency CHECK not validated")
	}
	if err := db.QueryRow(`SELECT attnotnull FROM pg_attribute WHERE attrelid = 'customer_credit_ledger'::regclass AND attname = 'currency'`).Scan(&notNull); err != nil {
		t.Fatal(err)
	}
	if !notNull {
		t.Error("00200 left customer_credit_ledger.currency nullable")
	}
	for _, bad := range []string{"NULL", "'usd'", "'US'"} {
		if err := execErr(`INSERT INTO customer_credit_ledger (id, tenant_id, customer_id, entry_type, currency, amount_cents, balance_after, description)
			VALUES ('ccl_bad', 'ten_ccy', 'cus_none', 'grant', ` + bad + `, 1, 1, 'bad')`); err == nil {
			t.Errorf("a ledger row with currency %s was accepted", bad)
		}
	}
	exec(`INSERT INTO customer_credit_ledger (id, tenant_id, customer_id, entry_type, currency, amount_cents, balance_after, description)
		VALUES ('ccl_ok', 'ten_ccy', 'cus_none', 'grant', 'CAD', 1, 1, 'ok')`)

	if _, err := down(ctx, dsn, migrationFiles(), creditLedgerCurrencyVersion-1); err != nil {
		t.Fatalf("Down to before %d: %v", creditLedgerCurrencyVersion, err)
	}
	var exists bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_name = 'customer_credit_ledger' AND column_name = 'currency')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("the Down left customer_credit_ledger.currency")
	}
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
}
