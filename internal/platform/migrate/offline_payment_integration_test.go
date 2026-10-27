package migrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// offlinePaymentVersion is 00203_offline_payment_no_payment_intent.sql;
// 00204 validates the CHECKs it adds.
const offlinePaymentVersion = 203

// 00203. Before it, an offline payment wrote "out_of_band:<paid_at>" into
// invoices.stripe_payment_intent_id and, through the settle's attempt
// upsert, a succeeded invoice_charge_attempts row under the same fake id.
// After it, that invoice has no PaymentIntent and paid_out_of_band is true;
// the fake attempt row is gone; nothing about what the invoice says changes
// (amounts, status, paid_at, updated_at); and the PaymentIntent columns
// refuse the prefix. Down puts the marker back for the code before it, and
// Up runs again.
func TestOfflinePayment_MarkerBecomesPaidOutOfBand(t *testing.T) {
	dsn, db := scratchDB(t, "velox_test_oob_b_migrate")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, offlinePaymentVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", offlinePaymentVersion, err)
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
			t.Fatalf("%s: %v", oneLine(q), err)
		}
	}
	exec(`INSERT INTO tenants (id, name) VALUES ('ten_oob', 'Offline')`)
	exec(`INSERT INTO customers (id, tenant_id, external_id, display_name) VALUES ('cus_oob', 'ten_oob', 'oob', 'OOB')`)
	exec(`INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, status, payment_status, currency,
			subtotal_cents, total_amount_cents, amount_due_cents, amount_paid_cents, billing_period_start, billing_period_end,
			paid_at, updated_at, stripe_payment_intent_id, charge_attempt_seq)
		VALUES
		('inv_oob', 'ten_oob', 'cus_oob', 'OOB-1', 'paid', 'succeeded', 'USD', 5362, 5362, 0, 5362,
			'2026-07-01', '2026-08-01', '2026-08-02T15:22:52Z', '2026-08-02T15:22:53Z', 'out_of_band:2026-08-02T15:22:52Z', 1),
		('inv_card', 'ten_oob', 'cus_oob', 'OOB-2', 'paid', 'succeeded', 'USD', 4500, 4500, 0, 4500,
			'2026-07-01', '2026-08-01', '2026-08-03T10:00:00Z', '2026-08-03T10:00:01Z', 'pi_card_1', 2),
		('inv_open', 'ten_oob', 'cus_oob', 'OOB-3', 'finalized', 'pending', 'USD', 700, 700, 700, 0,
			'2026-07-01', '2026-08-01', NULL, '2026-08-04T10:00:00Z', NULL, 0)`)
	exec(`INSERT INTO invoice_charge_attempts (tenant_id, invoice_id, stripe_payment_intent_id, trigger_source, outcome, amount_cents, occurred_at)
		VALUES
		('ten_oob', 'inv_oob', 'out_of_band:2026-08-02T15:22:52Z', 'external', 'succeeded', 5362, '2026-08-02T15:22:52Z'),
		('ten_oob', 'inv_card', 'pi_card_1', 'auto_charge', 'succeeded', 4500, '2026-08-03T10:00:00Z')`)

	// What each invoice says, and when it last changed. The migration must
	// leave all of it as it was.
	document := func() string {
		t.Helper()
		var s string
		if err := db.QueryRow(`SELECT string_agg(concat_ws('/', id, status, payment_status, subtotal_cents, total_amount_cents,
				amount_due_cents, amount_paid_cents, paid_at, updated_at, charge_attempt_seq), ' ' ORDER BY id) FROM invoices`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := document()

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	type row struct {
		pi      sql.NullString
		offline bool
	}
	read := func(id string) row {
		t.Helper()
		var r row
		if err := db.QueryRow(`SELECT stripe_payment_intent_id, paid_out_of_band FROM invoices WHERE id = $1`, id).Scan(&r.pi, &r.offline); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return r
	}
	for id, want := range map[string]row{
		"inv_oob":  {offline: true},
		"inv_card": {pi: sql.NullString{String: "pi_card_1", Valid: true}},
		"inv_open": {},
	} {
		if got := read(id); got != want {
			t.Errorf("%s after Up: PaymentIntent %v, paid_out_of_band %v; want %v, %v", id, got.pi, got.offline, want.pi, want.offline)
		}
	}
	if after := document(); after != before {
		t.Errorf("Up changed what an invoice says:\n before %s\n after  %s", before, after)
	}
	var attempts string
	if err := db.QueryRow(`SELECT string_agg(stripe_payment_intent_id, ',' ORDER BY stripe_payment_intent_id) FROM invoice_charge_attempts`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != "pi_card_1" {
		t.Errorf("charge attempts after Up: %q, want only the card's pi_card_1", attempts)
	}
	for _, name := range []string{
		"invoices_stripe_payment_intent_id_check",
		"invoices_paid_out_of_band_check",
		"invoice_charge_attempts_stripe_payment_intent_id_check",
	} {
		var validated bool
		if err := db.QueryRow(`SELECT convalidated FROM pg_constraint WHERE conname = $1`, name).Scan(&validated); err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !validated {
			t.Errorf("00204 left %s not validated", name)
		}
	}
	for _, bad := range []string{
		`UPDATE invoices SET stripe_payment_intent_id = 'out_of_band:2026-08-05T00:00:00Z' WHERE id = 'inv_open'`,
		`UPDATE invoices SET stripe_payment_intent_id = 'pi_late' WHERE id = 'inv_oob'`,
		`UPDATE invoices SET paid_out_of_band = true WHERE id = 'inv_card'`,
		`INSERT INTO invoice_charge_attempts (tenant_id, invoice_id, stripe_payment_intent_id, trigger_source, outcome, amount_cents, occurred_at)
			VALUES ('ten_oob', 'inv_open', 'out_of_band:2026-08-05T00:00:00Z', 'external', 'succeeded', 700, now())`,
	} {
		if err := execErr(bad); err == nil {
			t.Errorf("accepted: %s", oneLine(bad))
		} else if !strings.Contains(err.Error(), "check constraint") {
			t.Errorf("%s: %v, want a check constraint refusal", oneLine(bad), err)
		}
	}
	exec(`UPDATE invoices SET stripe_payment_intent_id = 'pi_open_1' WHERE id = 'inv_open'`)
	exec(`UPDATE invoices SET stripe_payment_intent_id = NULL WHERE id = 'inv_open'`)

	if _, err := down(ctx, dsn, migrationFiles(), offlinePaymentVersion-1); err != nil {
		t.Fatalf("Down to before %d: %v", offlinePaymentVersion, err)
	}
	var exists bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_name = 'invoices' AND column_name = 'paid_out_of_band')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("the Down left invoices.paid_out_of_band")
	}
	var marker sql.NullString
	if err := db.QueryRow(`SELECT stripe_payment_intent_id FROM invoices WHERE id = 'inv_oob'`).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if marker.String != "out_of_band:2026-08-02T15:22:52Z" {
		t.Errorf("the Down left inv_oob's PaymentIntent %v, want the marker the code before 00203 reads, out_of_band:2026-08-02T15:22:52Z", marker)
	}
	if after := document(); after != before {
		t.Errorf("Down changed what an invoice says:\n before %s\n after  %s", before, after)
	}
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
	if got := read("inv_oob"); got != (row{offline: true}) {
		t.Errorf("inv_oob after Up again: %v", got)
	}
}
