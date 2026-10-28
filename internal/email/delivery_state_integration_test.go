package email_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/email"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestDeliveryState_MonotonicConvergence locks ADR-098's ordering
// contract against real Postgres: webhook writes only promote
// (unknown < delivered < bounced < complained), so at-least-once
// redelivery and Delivery-vs-Bounce reordering converge to the
// most-severe state — no dedup table needed.
func TestDeliveryState_MonotonicConvergence(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Delivery State")
	store := email.NewOutboxStore(db)

	id, err := store.EnqueueStandalone(ctx, tenantID, email.TypeInvoice,
		map[string]any{"to": "a@b.test", "invoice_number": "INV-1"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	readState := func() string {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		var s string
		if err := tx.QueryRow(`SELECT delivery_state FROM email_outbox WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatalf("read state: %v", err)
		}
		return s
	}

	if got := readState(); got != email.DeliveryUnknown {
		t.Fatalf("fresh row: got %q, want unknown (0156 default)", got)
	}

	// GetByID resolves the row cross-tenant (the webhook's tenant
	// resolution) with tenant + livemode + payload intact.
	row, err := store.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if row.TenantID != tenantID || row.Payload["to"] != "a@b.test" || row.DeliveryState != email.DeliveryUnknown {
		t.Fatalf("GetByID row: %+v", row)
	}
	if _, err := store.GetByID(ctx, "hc_emob_ffffffffffffffffffffffff"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetByID missing: want sql.ErrNoRows, got %v", err)
	}

	// Promotion ladder with redelivery no-ops at every rung.
	steps := []struct {
		state    string
		wantsAny bool
		after    string
	}{
		{email.DeliveryDelivered, true, email.DeliveryDelivered},
		{email.DeliveryDelivered, false, email.DeliveryDelivered}, // redelivery
		{email.DeliveryBounced, true, email.DeliveryBounced},
		{email.DeliveryDelivered, false, email.DeliveryBounced}, // late Delivery: no downgrade
		{email.DeliveryComplained, true, email.DeliveryComplained},
		{email.DeliveryBounced, false, email.DeliveryComplained}, // late Bounce: no downgrade
		{"bogus", false, email.DeliveryComplained},               // unrecognized: fails closed
	}
	for i, s := range steps {
		updated, err := store.MarkDeliveryState(ctx, tenantID, id, s.state)
		if err != nil {
			t.Fatalf("step %d (%s): %v", i, s.state, err)
		}
		if updated != s.wantsAny {
			t.Errorf("step %d (%s): updated=%v, want %v", i, s.state, updated, s.wantsAny)
		}
		if got := readState(); got != s.after {
			t.Errorf("step %d (%s): state=%q, want %q", i, s.state, got, s.after)
		}
	}

	// The CHECK constraint is the last line of defense against a writer
	// bypassing the rank guard.
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.Exec(`UPDATE email_outbox SET delivery_state = 'bogus' WHERE id = $1`, id); err == nil {
		t.Error("CHECK constraint must reject an unknown delivery_state")
	}
}
