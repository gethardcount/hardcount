package customer_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestSetStripeCustomerIDAudit_SharedFate pins the ADR-090 emission hook on
// PostgresStore.SetStripeCustomerIDAudited — the checkout.session.completed
// payment-setup flip, a BACKGROUND webhook mutation that previously left no
// audit trail at all, and that shipped with no fault injection.
//
// The three invariants, in order of what the review flagged:
//
//  1. a MATCHED customer (rowsAffected==1) emits once and commits the mapping;
//  2. a NONEXISTENT customer id (zero-row UPDATE) emits NOTHING and returns nil
//     — the FABRICATED-EVIDENCE class: the hook used to write a
//     `payment_setup_completed` row for a customer whose row was never written
//     (torn down by a test-clock delete, or on the other livemode plane under
//     RLS), permanently recording a mutation that never happened in an
//     append-only compliance log;
//  3. an emit failure ABORTS the mapping write (shared fate) — no
//     stripe_customer_id lands unrecorded.
//
// Mutation-verify: drop the `n == 1` guard and (2) fails; drop the emit-error
// propagation and (3) fails.
func TestSetStripeCustomerIDAudit_SharedFate(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()
	tenantID := testutil.CreateTestTenant(t, db, "Stripe Mapping InTx Audit")

	store := customer.NewPostgresStore(db)
	logger := audit.NewLogger(db)

	// The emission the webhook path builds (payment_setup_completed on the
	// customer), instrumented with a call counter.
	emitFor := func(custID string, calls *int) func(*sql.Tx) error {
		return func(tx *sql.Tx) error {
			*calls++
			return logger.LogInTx(ctx, tx, audit.Entry{
				Action:       domain.AuditActionUpdate,
				ResourceType: "customer",
				ResourceID:   custID,
				Metadata: map[string]any{
					"action":             "payment_setup_completed",
					"stripe_customer_id": "cus_stripe_" + custID[len(custID)-6:],
				},
			})
		}
	}

	seed := func(t *testing.T, external string) domain.Customer {
		t.Helper()
		c, err := store.Create(ctx, tenantID, domain.Customer{
			ExternalID: external, DisplayName: "Mapping Co",
		})
		if err != nil {
			t.Fatalf("seed customer: %v", err)
		}
		return c
	}

	setupRows := func(t *testing.T, custID string) []domain.AuditEntry {
		t.Helper()
		rows, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{
			ResourceType: "customer", ResourceID: custID,
		})
		if err != nil {
			t.Fatalf("query audit: %v", err)
		}
		var out []domain.AuditEntry
		for _, r := range rows {
			if r.Metadata["action"] == "payment_setup_completed" {
				out = append(out, r)
			}
		}
		return out
	}

	t.Run("matched customer emits once and commits the mapping", func(t *testing.T) {
		c := seed(t, "cus_mapped")

		calls := 0
		if err := store.SetStripeCustomerIDAudited(ctx, tenantID, c.ID, "cus_stripe_mapped", emitFor(c.ID, &calls)); err != nil {
			t.Fatalf("set stripe customer id: %v", err)
		}
		if calls != 1 {
			t.Fatalf("emit calls = %d, want exactly 1", calls)
		}
		rows := setupRows(t, c.ID)
		if len(rows) != 1 {
			t.Fatalf("want exactly one payment_setup_completed audit row; got %d: %+v", len(rows), rows)
		}
		if rows[0].Action != domain.AuditActionUpdate || rows[0].ResourceType != "customer" {
			t.Errorf("audit row vocabulary: got %s/%s", rows[0].Action, rows[0].ResourceType)
		}
		got, err := store.Get(ctx, tenantID, c.ID)
		if err != nil {
			t.Fatalf("get customer: %v", err)
		}
		if got.StripeCustomerID != "cus_stripe_mapped" {
			t.Errorf("stripe_customer_id = %q, want cus_stripe_mapped — the mapping did not commit with its audit row", got.StripeCustomerID)
		}
	})

	// A zero-row UPDATE means the customer isn't there to map. It must emit
	// NOTHING (evidence of a mutation that never happened) and must REPORT the
	// miss rather than succeed silently — a silent success let the operator
	// route hand back a Stripe session for a customer Hardcount does not have, and
	// left the request with zero audit rows once the handler suppressed the
	// catch-all. The two callers then diverge on purpose: the operator route
	// 404s; the webhook acks (see payment.handleCheckoutCompleted).
	t.Run("nonexistent customer emits nothing and reports ErrNotFound", func(t *testing.T) {
		ghostID := postgres.NewID("hc_cus") // never inserted

		calls := 0
		err := store.SetStripeCustomerIDAudited(ctx, tenantID, ghostID, "cus_stripe_ghost", emitFor(ghostID, &calls))
		if !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("zero-row UPDATE must report ErrNotFound, not silent success; got %v", err)
		}
		if calls != 0 {
			t.Fatalf("emit fired %d time(s) for a customer whose row was never written — FABRICATED EVIDENCE", calls)
		}
		rows, _, qErr := logger.Query(ctx, tenantID, audit.QueryFilter{
			ResourceType: "customer", ResourceID: ghostID,
		})
		if qErr != nil {
			t.Fatalf("query audit: %v", qErr)
		}
		if len(rows) != 0 {
			t.Errorf("audit rows recorded against a nonexistent customer: %+v", rows)
		}
	})

	t.Run("emit failure rolls the mapping write back", func(t *testing.T) {
		c := seed(t, "cus_rollback")

		calls := 0
		inner := emitFor(c.ID, &calls)
		// Land the row, THEN fail: proves the audit row rolls back WITH the
		// mapping, not merely that a never-attempted write left nothing.
		err := store.SetStripeCustomerIDAudited(ctx, tenantID, c.ID, "cus_stripe_rollback", func(tx *sql.Tx) error {
			if err := inner(tx); err != nil {
				return err
			}
			return errors.New("injected audit failure")
		})
		if err == nil {
			t.Fatal("mapping write must fail when its audit emission fails (shared fate)")
		}
		if calls != 1 {
			t.Fatalf("emit calls = %d, want 1", calls)
		}
		if rows := setupRows(t, c.ID); len(rows) != 0 {
			t.Errorf("audit row survived the rolled-back mapping write: %+v", rows)
		}
		got, err := store.Get(ctx, tenantID, c.ID)
		if err != nil {
			t.Fatalf("get customer: %v", err)
		}
		if got.StripeCustomerID != "" {
			t.Errorf("stripe_customer_id = %q, want empty — the mapping committed despite a failed audit emission", got.StripeCustomerID)
		}
		// And the write is still available to a retry (webhook redelivery).
		if err := store.SetStripeCustomerIDAudited(ctx, tenantID, c.ID, "cus_stripe_rollback", nil); err != nil {
			t.Fatalf("retry mapping write: %v", err)
		}
		got, err = store.Get(ctx, tenantID, c.ID)
		if err != nil {
			t.Fatalf("get customer after retry: %v", err)
		}
		if got.StripeCustomerID != "cus_stripe_rollback" {
			t.Errorf("stripe_customer_id after retry = %q, want cus_stripe_rollback", got.StripeCustomerID)
		}
	})
}
