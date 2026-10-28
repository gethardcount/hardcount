package customer_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestCustomersEmail_NotNull verifies the HYG-1 DB constraint: customers.email
// is NOT NULL DEFAULT ”. Empty-string emails are allowed (email is optional
// at the API layer), but a raw NULL must be rejected by Postgres regardless
// of what the application thinks it is writing.
func TestCustomersEmail_NotNull(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := customer.NewPostgresStore(db)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 10*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Email NotNull")

	// Empty-string email must round-trip as empty string (not NULL).
	c, err := store.Create(ctx, tenantID, domain.Customer{
		ExternalID:  "cus_empty_email",
		DisplayName: "No Email Co",
		Email:       "",
	})
	if err != nil {
		t.Fatalf("create with empty email: %v", err)
	}
	if c.Email != "" {
		t.Errorf("empty-email round-trip: got %q, want empty", c.Email)
	}

	// Raw NULL via direct SQL (bypassing the store) must be rejected.
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', 'on', true)`); err != nil {
		t.Fatalf("set livemode: %v", err)
	}
	defer postgres.Rollback(tx)

	_, err = tx.ExecContext(ctx,
		`INSERT INTO customers (id, tenant_id, external_id, display_name, email, status)
		 VALUES ('hc_cus_nulltest', $1, 'cus_null_email', 'Null Email Co', NULL, 'active')`,
		tenantID,
	)
	if err == nil {
		t.Fatal("expected NOT NULL violation on NULL email, got nil error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "null") {
		t.Errorf("error did not mention NULL constraint: %v", err)
	}
}
