package payment

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

// mappingSetupStore is the DURABLE slice of the production composite store
// (internal/api/adapters.go, compositePaymentSetupStore): post-migration-0097
// the one write that survives on the /v1/checkout/setup path is
// customers.stripe_customer_id, and the Audited variant threads the ADR-090
// emission onto THAT write's tx. It delegates to the real
// customer.PostgresStore method — so this test exercises the production
// rowsAffected==1 gate, not a re-implementation of it.
type mappingSetupStore struct{ customers *customer.PostgresStore }

func (s *mappingSetupStore) UpsertPaymentSetup(ctx context.Context, tenantID string, ps domain.CustomerPaymentSetup) (domain.CustomerPaymentSetup, error) {
	return s.UpsertPaymentSetupAudited(ctx, tenantID, ps, nil)
}

func (s *mappingSetupStore) UpsertPaymentSetupAudited(ctx context.Context, tenantID string, ps domain.CustomerPaymentSetup, emit func(tx *sql.Tx) error) (domain.CustomerPaymentSetup, error) {
	if ps.StripeCustomerID != "" && ps.CustomerID != "" {
		if err := s.customers.SetStripeCustomerIDAudited(ctx, tenantID, ps.CustomerID, ps.StripeCustomerID, emit); err != nil {
			return domain.CustomerPaymentSetup{}, err
		}
	}
	return s.GetPaymentSetup(ctx, tenantID, ps.CustomerID)
}

func (s *mappingSetupStore) GetPaymentSetup(ctx context.Context, tenantID, customerID string) (domain.CustomerPaymentSetup, error) {
	c, err := s.customers.Get(ctx, tenantID, customerID)
	if err != nil {
		return domain.CustomerPaymentSetup{}, err
	}
	return domain.CustomerPaymentSetup{
		CustomerID: customerID, TenantID: tenantID, StripeCustomerID: c.StripeCustomerID,
	}, nil
}

// TestCheckoutSetupAudit_SharedFate pins the ADR-090 emission on
// POST /v1/checkout/setup (checkout.go → persistStripeMapping). The route has
// no service and owns no tx: the emission rides the customer↔Stripe mapping
// write — the only durable LOCAL mutation it makes — and the request now fails
// when that write (or its evidence) fails, where it used to discard the error.
//
// Invariants:
//
//  1. a real customer → the mapping commits and exactly ONE
//     checkout_setup_started row lands, carrying the Stripe customer id;
//  2. an emit failure ABORTS the mapping write (shared fate) — no
//     stripe_customer_id can commit unrecorded, and no row survives the abort;
//  3. an UNKNOWN customer id (zero-row UPDATE) emits NOTHING — the
//     fabricated-evidence class the middleware catch-all was deleted for (it
//     would have recorded "updated customer {id}" for a customer that was
//     never touched).
//
// Mutation-verify: return nil instead of the error in persistStripeMapping's
// caller and (2) still passes but the route re-opens the unrecorded-mutation
// hole — so (2) asserts the STORE-level rollback, not just the HTTP code; drop
// the `n == 1` gate in customer.SetStripeCustomerIDAudited and (3) fails.
func TestCheckoutSetupAudit_SharedFate(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()
	tenantID := testutil.CreateTestTenant(t, db, "Checkout Setup InTx Audit")

	customers := customer.NewPostgresStore(db)
	logger := audit.NewLogger(db)
	store := &mappingSetupStore{customers: customers}

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
			if r.Metadata["action"] == "checkout_setup_started" {
				out = append(out, r)
			}
		}
		return out
	}

	seed := func(t *testing.T, external string) domain.Customer {
		t.Helper()
		c, err := customers.Create(ctx, tenantID, domain.Customer{
			ExternalID: external, DisplayName: "Checkout Co",
		})
		if err != nil {
			t.Fatalf("seed customer: %v", err)
		}
		return c
	}

	t.Run("setup on a real customer commits the mapping with one row", func(t *testing.T) {
		c := seed(t, "cus_setup_ok")
		h := &CheckoutHandler{store: store, audit: logger}

		req := SetupRequest{CustomerID: c.ID, CustomerName: "Checkout Co"}
		if err := h.persistStripeMapping(ctx, tenantID, req, "cus_stripe_ok"); err != nil {
			t.Fatalf("persist stripe mapping: %v", err)
		}

		rows := setupRows(t, c.ID)
		if len(rows) != 1 {
			t.Fatalf("want exactly one checkout_setup_started row; got %d: %+v", len(rows), rows)
		}
		r := rows[0]
		if r.Action != domain.AuditActionUpdate || r.ResourceType != "customer" || r.ResourceID != c.ID {
			t.Errorf("row vocabulary = %s/%s/%s, want update/customer/%s", r.Action, r.ResourceType, r.ResourceID, c.ID)
		}
		if r.Metadata["stripe_customer_id"] != "cus_stripe_ok" {
			t.Errorf("stripe_customer_id in metadata = %v, want cus_stripe_ok", r.Metadata["stripe_customer_id"])
		}
		// No label by design: the only name available here is the REQUEST's
		// claim, and audit_log is append-only — a caller sending a name that
		// doesn't match the stored customer would permanently label the row
		// with a value nothing verified. The dashboard falls back to the
		// resource type and deep-links resource_id.
		if r.ResourceLabel != "" {
			t.Errorf("resource_label = %q, want empty — unverified request input must not enter a permanent record", r.ResourceLabel)
		}
		got, err := customers.Get(ctx, tenantID, c.ID)
		if err != nil {
			t.Fatalf("get customer: %v", err)
		}
		if got.StripeCustomerID != "cus_stripe_ok" {
			t.Errorf("stripe_customer_id = %q, want cus_stripe_ok — the mapping did not commit with its audit row", got.StripeCustomerID)
		}
	})

	t.Run("emit failure rolls the mapping write back", func(t *testing.T) {
		c := seed(t, "cus_setup_rollback")
		emitter := &failingCheckoutEmitter{inner: logger}
		h := &CheckoutHandler{store: store, audit: emitter}

		req := SetupRequest{CustomerID: c.ID, CustomerName: "Checkout Co"}
		err := h.persistStripeMapping(ctx, tenantID, req, "cus_stripe_rollback")
		if err == nil {
			t.Fatal("the mapping write must fail when its audit emission fails (shared fate)")
		}
		if emitter.calls != 1 {
			t.Fatalf("emit calls = %d, want 1", emitter.calls)
		}
		if rows := setupRows(t, c.ID); len(rows) != 0 {
			t.Errorf("audit row survived the rolled-back mapping write: %+v", rows)
		}
		got, err := customers.Get(ctx, tenantID, c.ID)
		if err != nil {
			t.Fatalf("get customer: %v", err)
		}
		if got.StripeCustomerID != "" {
			t.Errorf("stripe_customer_id = %q, want empty — the mapping committed despite a failed audit emission", got.StripeCustomerID)
		}
	})

	t.Run("unknown customer emits nothing", func(t *testing.T) {
		ghostID := postgres.NewID("vlx_cus") // never inserted
		counting := &countingCheckoutEmitter{inner: logger}
		h := &CheckoutHandler{store: store, audit: counting}

		req := SetupRequest{CustomerID: ghostID, CustomerName: "Ghost Co"}
		err := h.persistStripeMapping(ctx, tenantID, req, "cus_stripe_ghost")
		if !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("a setup for a customer that does not exist must fail not-found, got %v", err)
		}
		if counting.calls != 0 {
			t.Fatalf("emit fired %d time(s) for a customer whose row was never written — FABRICATED EVIDENCE", counting.calls)
		}
		if rows := setupRows(t, ghostID); len(rows) != 0 {
			t.Errorf("audit rows recorded against a nonexistent customer: %+v", rows)
		}
	})
}

// failingCheckoutEmitter lands the real row and THEN fails — proving the audit
// row rolls back WITH the mapping, not merely that a never-attempted write left
// nothing behind.
type failingCheckoutEmitter struct {
	inner *audit.Logger
	calls int
}

func (f *failingCheckoutEmitter) LogInTx(ctx context.Context, tx *sql.Tx, e audit.Entry) error {
	f.calls++
	if err := f.inner.LogInTx(ctx, tx, e); err != nil {
		return err
	}
	return errors.New("injected audit failure")
}

type countingCheckoutEmitter struct {
	inner *audit.Logger
	calls int
}

func (c *countingCheckoutEmitter) LogInTx(ctx context.Context, tx *sql.Tx, e audit.Entry) error {
	c.calls++
	return c.inner.LogInTx(ctx, tx, e)
}

// A checkout.session.completed for a customer that no longer exists (hard
// deleted by a test-clock teardown, or on the other livemode plane than the
// webhook) must ACK — not error. Returning an error here would loop Stripe's
// redelivery for days against a customer Velox will never have again. The
// operator route takes the OPPOSITE branch on the same store error and 404s;
// that divergence is the point of the store reporting rather than deciding.
func TestCheckoutCompleted_MissingCustomer_AcksAndEmitsNothing(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()
	tenantID := testutil.CreateTestTenant(t, db, "Checkout Completed Ghost")

	customers := customer.NewPostgresStore(db)
	logger := audit.NewLogger(db)
	store := &mappingSetupStore{customers: customers}

	ghostID := postgres.NewID("vlx_cus") // never inserted

	calls := 0
	emit := func(tx *sql.Tx) error {
		calls++
		return logger.LogInTx(ctx, tx, audit.Entry{
			Action:       domain.AuditActionUpdate,
			ResourceType: "customer",
			ResourceID:   ghostID,
			Metadata:     map[string]any{"action": "payment_setup_completed"},
		})
	}

	_, err := store.UpsertPaymentSetupAudited(ctx, tenantID, domain.CustomerPaymentSetup{
		CustomerID:       ghostID,
		TenantID:         tenantID,
		StripeCustomerID: "cus_stripe_ghost",
	}, emit)

	// The store REPORTS the miss (the webhook handler is what swallows it).
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("mapping write for a missing customer must report ErrNotFound; got %v", err)
	}
	if calls != 0 {
		t.Errorf("emit fired %d time(s) for a customer that does not exist — fabricated evidence", calls)
	}
	rows, _, qErr := logger.Query(ctx, tenantID, audit.QueryFilter{
		ResourceType: "customer", ResourceID: ghostID,
	})
	if qErr != nil {
		t.Fatalf("query audit: %v", qErr)
	}
	if len(rows) != 0 {
		t.Errorf("audit rows for a customer that never existed: %+v", rows)
	}
}
