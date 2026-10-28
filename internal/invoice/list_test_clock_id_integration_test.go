package invoice_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestListAndGet_CarryTheCustomersTestClock: the invoice list and the invoice
// read name the test clock an invoice's dates follow, so the dashboard reads
// "Due in N days" from that clock without joining subscriptions itself (sweep
// D-S1: the join it did was capped at 50 subscriptions). The pin lives on the
// customer, is immutable, and a subscription inherits it (ADR-027), so the
// customer's clock covers cycle and one-off invoices alike.
//
// Three rows, one per shape:
//   - cycle invoice on the pinned customer   -> the clock id
//   - one-off invoice on the pinned customer -> the clock id
//   - one-off invoice on a wall customer     -> no test_clock_id key at all
func TestListAndGet_CarryTheCustomersTestClock(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := invoice.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)

	const tenantID = "hc_ten_inv_clk_id"
	const clockID = "hc_tclk_inv_clk_id"

	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// TxBypass sets no app.livemode; pin test mode so the livemode trigger
	// does not stamp the rows live and trip the test-clock CHECKs.
	mustExec(`SELECT set_config('app.livemode', 'off', true)`)
	mustExec(`INSERT INTO tenants (id, name, status) VALUES ($1, 'Invoice Clock Id', 'active')`, tenantID)
	mustExec(`INSERT INTO test_clocks (id, tenant_id, name, frozen_time, status, livemode)
		VALUES ($1, $2, 'inv-clk', now(), 'ready', false)`, clockID, tenantID)
	mustExec(`INSERT INTO customers (id, tenant_id, external_id, display_name, email, test_clock_id, created_at, updated_at)
		VALUES ('cus_inv_clk_pinned', $1, 'pinned', 'Pinned', '', $2, now(), now())`, tenantID, clockID)
	mustExec(`INSERT INTO customers (id, tenant_id, external_id, display_name, email, created_at, updated_at)
		VALUES ('cus_inv_clk_wall', $1, 'wall', 'Wall', '', now(), now())`, tenantID)
	mustExec(`INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status, livemode, test_clock_id)
		VALUES ('sub_inv_clk_pinned', $1, 'pinned', 'pinned', 'cus_inv_clk_pinned', 'active', false, $2)`, tenantID, clockID)
	seedInv := func(id, custID, subID string, simulated bool) {
		sub := any(nil)
		if subID != "" {
			sub = subID
		}
		mustExec(`INSERT INTO invoices (id, tenant_id, customer_id, subscription_id, invoice_number,
			status, payment_status, is_simulated, currency,
			subtotal_cents, total_amount_cents, amount_due_cents, tax_status,
			billing_period_start, billing_period_end, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $1, 'finalized', 'pending', $5, 'USD',
			2500, 2500, 2500, 'ok', now(), now(), now(), now())`, id, tenantID, custID, sub, simulated)
	}
	seedInv("inv_clk_cycle", "cus_inv_clk_pinned", "sub_inv_clk_pinned", true)
	seedInv("inv_clk_oneoff", "cus_inv_clk_pinned", "", true)
	seedInv("inv_clk_wall", "cus_inv_clk_wall", "", false)
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	want := map[string]string{
		"inv_clk_cycle":  clockID,
		"inv_clk_oneoff": clockID,
		"inv_clk_wall":   "",
	}
	// clockOf reads the field from the JSON the API sends, so the test holds
	// the wire shape: the key is present with the id, or absent.
	clockOf := func(v any) (string, bool) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		raw, ok := m["test_clock_id"]
		if !ok {
			return "", false
		}
		s, _ := raw.(string)
		return s, true
	}
	check := func(route, id string, v any) {
		t.Helper()
		got, present := clockOf(v)
		switch w := want[id]; {
		case w == "" && present:
			t.Errorf("%s %s: test_clock_id = %q, want the key left out (wall-clock customer)", route, id, got)
		case w != "" && got != w:
			t.Errorf("%s %s: test_clock_id = %q (present %v), want %q", route, id, got, present, w)
		}
	}

	list, total, err := store.List(ctx, invoice.ListFilter{TenantID: tenantID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 3 || len(list) != 3 {
		t.Fatalf("list returned %d rows (total %d), want 3", len(list), total)
	}
	for _, inv := range list {
		check("GET /v1/invoices", inv.ID, inv)
	}

	for id := range want {
		inv, err := store.Get(ctx, tenantID, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		check("GET /v1/invoices/{id}", id, inv)
	}
}
