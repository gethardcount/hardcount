package migrate

import (
	"context"
	"testing"
)

// oneRetryPolicyVersion is 00248_one_retry_policy.sql.
const oneRetryPolicyVersion = 248

// oneRetryPolicySeed is one account and mode with two dunning policies: the
// older one not the default, with a customer assigned to it and an active
// run on it, and the default created after it.
const oneRetryPolicySeed = `
INSERT INTO tenants (id, name) VALUES ('ten_orp', 'One retry policy');
INSERT INTO dunning_policies (id, tenant_id, name, retry_days, is_default, created_at)
	VALUES ('dpol_orp_other', 'ten_orp', 'Other', '[1]', false, now() - interval '2 days'),
	       ('dpol_orp_default', 'ten_orp', 'Default', '[3, 4, 7]', true, now() - interval '1 day');
INSERT INTO customers (id, tenant_id, external_id, display_name, dunning_policy_id)
	VALUES ('cus_orp', 'ten_orp', 'orp', 'ORP', 'dpol_orp_other');
INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, status, payment_status, currency,
		subtotal_cents, total_amount_cents, amount_due_cents, billing_period_start, billing_period_end)
	VALUES ('inv_orp', 'ten_orp', 'cus_orp', 'ORP-1', 'finalized', 'failed', 'USD', 1000, 1000, 1000, now() - interval '30 days', now());
INSERT INTO invoice_dunning_runs (id, tenant_id, invoice_id, customer_id, policy_id, state, attempt_count)
	VALUES ('drun_orp', 'ten_orp', 'inv_orp', 'cus_orp', 'dpol_orp_other', 'active', 1);
`

// TestOneRetryPolicy_KeepsTheDefault: the Up keeps one policy per account
// and mode, the default even when another is older, and deletes the other.
// The customer assigned to the deleted policy and the run on it stay, and no
// longer name a policy. The policy's naming columns go, and a second policy
// in the mode is refused. The Down puts the columns back with the kept policy
// as the default and the run pointing at it; the Up runs again.
func TestOneRetryPolicy_KeepsTheDefault(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_test_one_retry_policy")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, oneRetryPolicyVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", oneRetryPolicyVersion, err)
	}
	if err := deadContractExec(ctx, db, oneRetryPolicySeed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if got := noDraftScalar[string](t, db, `SELECT string_agg(id, ',' ORDER BY id) FROM dunning_policies WHERE tenant_id = 'ten_orp'`); got != "dpol_orp_default" {
		t.Errorf("policies after Up = %s, want only the default dpol_orp_default", got)
	}
	if got := noDraftScalar[string](t, db, `SELECT state || ':' || attempt_count FROM invoice_dunning_runs WHERE id = 'drun_orp'`); got != "active:1" {
		t.Errorf("run after Up = %s, want active:1, unchanged", got)
	}
	if n := noDraftScalar[int](t, db, `SELECT count(*) FROM customers WHERE id = 'cus_orp'`); n != 1 {
		t.Errorf("customers after Up = %d, want the assigned customer kept", n)
	}
	for _, col := range [][2]string{
		{"customers", "dunning_policy_id"}, {"invoice_dunning_runs", "policy_id"},
		{"dunning_policies", "name"}, {"dunning_policies", "is_default"}, {"dunning_policies", "seeded"},
	} {
		if columnExists(t, db, col[0], col[1]) {
			t.Errorf("after Up, %s still has %s", col[0], col[1])
		}
	}
	if err := deadContractExec(ctx, db, `INSERT INTO dunning_policies (id, tenant_id, retry_days) VALUES ('dpol_orp_second', 'ten_orp', '[2]')`); err == nil {
		t.Error("after Up, a second policy in the same account and mode was accepted")
	}

	if _, err := down(ctx, dsn, migrationFiles(), oneRetryPolicyVersion-1); err != nil {
		t.Fatalf("Down to %d: %v", oneRetryPolicyVersion-1, err)
	}
	if got := noDraftScalar[string](t, db, `SELECT id || ':' || name || ':' || is_default::text || ':' || seeded::text FROM dunning_policies WHERE tenant_id = 'ten_orp'`); got != "dpol_orp_default:Retries:true:false" {
		t.Errorf("policy after Down = %s, want dpol_orp_default:Retries:true:false", got)
	}
	if got := noDraftScalar[string](t, db, `SELECT policy_id FROM invoice_dunning_runs WHERE id = 'drun_orp'`); got != "dpol_orp_default" {
		t.Errorf("run's policy_id after Down = %s, want the kept policy", got)
	}
	if n := noDraftScalar[int](t, db, `SELECT count(*) FROM customers WHERE dunning_policy_id IS NOT NULL`); n != 0 {
		t.Errorf("customers assigned a policy after Down = %d, want 0", n)
	}
	if !indexExists(t, db, "idx_dunning_policies_one_default_per_tenant") || !indexExists(t, db, "idx_customers_dunning_policy_id") {
		t.Error("after Down, an index is missing")
	}

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
	if !indexExists(t, db, "idx_dunning_policies_one_per_mode") {
		t.Error("after the second Up, idx_dunning_policies_one_per_mode is missing")
	}
}
