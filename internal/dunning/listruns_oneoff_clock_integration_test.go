package dunning_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/dunning"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestListRuns_OneOffInvoiceRunOnACustomerClock: a dunning run on a one-off
// invoice (subscription_id NULL) of a clock-pinned customer resolves its
// clock through the customer (the pin's owner, ADR-027):
//   - ListRuns surfaces effective_now + test_clock_id for it (the sub join
//     rendered it wall-anchored and badge-less);
//   - ListEvents tie-breaks equal created_at by id (creation-ordered xid),
//     so same-sim-instant cascades render deterministically causal.
func TestListRuns_OneOffInvoiceRunOnACustomerClock(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	const tenantID = "hc_ten_dun_oneoff"
	const clockID = "hc_tclk_dun_oneoff"
	frozen := time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)

	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// 0021 livemode-autoset trigger reads the GUC; pin the tx to test mode.
	mustExec(`SELECT set_config('app.livemode', 'off', true)`)
	mustExec(`INSERT INTO tenants (id, name, status) VALUES ($1, 'Dun OneOff', 'active')`, tenantID)
	mustExec(`INSERT INTO test_clocks (id, tenant_id, name, frozen_time, status, livemode)
		VALUES ($1, $2, 'dunoneoff', $3, 'ready', false)`, clockID, tenantID, frozen)
	mustExec(`INSERT INTO customers (id, tenant_id, external_id, display_name, email, test_clock_id, created_at, updated_at)
		VALUES ('cus_dun_pin', $1, 'dun_pin', 'Pin', '', $2, $3, $3)`, tenantID, clockID, frozen)
	mustExec(`INSERT INTO dunning_policies (id, tenant_id, name, enabled, is_default, retry_days, final_subscription_action, final_invoice_action, livemode)
		VALUES ('dpol_oneoff', $1, 'P', true, true, '[3, 3, 3]', 'pause', 'none', false)`, tenantID)
	// A one-off invoice: subscription_id NULL.
	mustExec(`INSERT INTO invoices (id, tenant_id, customer_id, invoice_number,
			status, payment_status, is_simulated, currency, subtotal_cents, total_amount_cents,
			amount_due_cents, tax_status, billing_period_start, billing_period_end, created_at, updated_at)
		VALUES ('inv_dun_oneoff', $1, 'cus_dun_pin', 'inv_dun_oneoff', 'finalized', 'failed', true, 'USD', 2500, 2500, 2500, 'ok', $2, $2, $2, $2)`,
		tenantID, frozen.Add(-24*time.Hour))
	mustExec(`INSERT INTO invoice_dunning_runs (id, tenant_id, invoice_id, customer_id, policy_id, state, reason, attempt_count, next_action_at, paused, created_at, updated_at, livemode)
		VALUES ('run_oneoff_pin', $1, 'inv_dun_oneoff', 'cus_dun_pin', 'dpol_oneoff', 'active', 'payment_failed', 0, $2, false, $3, $3, false)`,
		tenantID, frozen.Add(-time.Hour), frozen.Add(-24*time.Hour))
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	dstore := dunning.NewPostgresStore(db)

	t.Run("ListRuns anchors one-off runs on the customer clock", func(t *testing.T) {
		runs, _, err := dstore.ListRuns(ctx, dunning.RunListFilter{TenantID: tenantID})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		var oneoff *domain.InvoiceDunningRun
		for i := range runs {
			if runs[i].ID == "run_oneoff_pin" {
				oneoff = &runs[i]
			}
		}
		if oneoff == nil {
			t.Fatal("run_oneoff_pin missing from ListRuns")
		}
		if oneoff.EffectiveNow == nil || !oneoff.EffectiveNow.Equal(frozen) {
			t.Errorf("effective_now: got %v, want the customer clock's frozen_time %v (was NULL under the sub join — wall-anchored relative times)", oneoff.EffectiveNow, frozen)
		}
		if oneoff.TestClockID != clockID {
			t.Errorf("test_clock_id: got %q, want %q (drives the badge server-authoritatively)", oneoff.TestClockID, clockID)
		}
	})

	t.Run("ListEvents tie-breaks equal created_at by id", func(t *testing.T) {
		sameInstant := frozen.Add(-time.Hour)
		tx2, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx2)
		// ids chosen so lexical id order == causal order (xid ids are
		// creation-ordered in production; here we pin it explicitly).
		for _, ev := range []struct{ id, typ string }{
			{"devt_a_started", "dunning_started"},
			{"devt_b_retry", "retry_attempted"},
		} {
			if _, err := tx2.ExecContext(ctx, `INSERT INTO invoice_dunning_events (id, run_id, tenant_id, invoice_id, event_type, state, attempt_count, created_at, livemode)
				VALUES ($1, 'run_oneoff_pin', $2, 'inv_dun_oneoff', $3, 'active', 0, $4, false)`,
				ev.id, tenantID, ev.typ, sameInstant); err != nil {
				t.Fatalf("seed event: %v", err)
			}
		}
		if err := tx2.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		evts, err := dstore.ListEvents(ctx, tenantID, "run_oneoff_pin")
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		if len(evts) != 2 {
			t.Fatalf("events: got %d, want 2", len(evts))
		}
		if evts[0].EventType != "dunning_started" || evts[1].EventType != "retry_attempted" {
			t.Errorf("equal created_at must order by id (causal): got %s then %s", evts[0].EventType, evts[1].EventType)
		}
	})
}
