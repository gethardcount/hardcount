package audit_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/crypto"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestActorName_IsDecryptedForCustomerActorRows
//
// audit_log does not store actor_name — it RESOLVES it at read time by LEFT JOIN:
// an api_keys name, a customers display_name, or a users email. But
// customers.display_name is ENCRYPTED AT REST, and the audit reader had no
// encryptor. So with HARDCOUNT_ENCRYPTION_KEY set — i.e. in production — every
// customer-actor row (a hosted-invoice Pay click, a payment-update link click)
// showed its Actor as CIPHERTEXT: on the dashboard, and in the CSV export handed
// to an auditor. "Who did this" is the audit log's first job, and for the one
// actor type that is a real human customer it was answering in gibberish.
//
// Both readers matter and are asserted here: Query (the dashboard) and Stream
// (the CSV export) go through the same scan path, so a fix that missed one would
// leave the auditor's FILE wrong while the screen looked right.
func TestActorName_IsDecryptedForCustomerActorRows(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Actor Name Crypto")

	// A real key, wired exactly as router.go wires it.
	enc, err := crypto.NewEncryptor(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("new encryptor: %v", err)
	}

	custStore := customer.NewPostgresStore(db)
	custStore.SetEncryptor(enc)

	const displayName = "Wayne Enterprises"
	cust, err := custStore.Create(ctx, tenantID, domain.Customer{
		ExternalID:  "cus_actor_crypto",
		DisplayName: displayName,
		Email:       "ceo@wayne.test",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	// Prove the fixture actually encrypts, or the test asserts nothing.
	var stored string
	if err := db.WithTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`SELECT display_name FROM customers WHERE id = $1`, cust.ID).Scan(&stored)
	}); err != nil {
		t.Fatalf("read raw display_name: %v", err)
	}
	if stored == displayName {
		t.Fatalf("fixture is not encrypting display_name (stored %q) — this test would pass even unfixed", stored)
	}

	logger := audit.NewLogger(db)
	logger.SetEncryptor(enc)

	// The row a customer writes: a hosted-invoice Pay click.
	if err := logger.Log(
		postgres.WithLivemode(auth.WithCustomerActor(ctx, cust.ID), false),
		tenantID, domain.AuditActionUpdate, "invoice", "hc_inv_actor", "INV-1",
		map[string]any{"action": "hosted_checkout_started"},
	); err != nil {
		t.Fatalf("log: %v", err)
	}

	t.Run("Query (the dashboard) shows the name, not ciphertext", func(t *testing.T) {
		rows, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{ResourceType: "invoice"})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("rows: got %d, want 1", len(rows))
		}
		assertActorName(t, rows[0].ActorName, displayName)
	})

	t.Run("Stream (the CSV an auditor receives) shows the name too", func(t *testing.T) {
		var got []domain.AuditEntry
		if err := logger.Stream(ctx, tenantID, audit.QueryFilter{ResourceType: "invoice"},
			func(e domain.AuditEntry) error { got = append(got, e); return nil }); err != nil {
			t.Fatalf("stream: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("streamed rows: got %d, want 1", len(got))
		}
		assertActorName(t, got[0].ActorName, displayName)
	})
}

func assertActorName(t *testing.T, got, want string) {
	t.Helper()
	if strings.HasPrefix(got, "enc:") {
		t.Fatalf("actor_name is CIPHERTEXT (%q) — the Actor column of every customer-actor row is gibberish, on screen and in the exported evidence", got)
	}
	if got != want {
		t.Errorf("actor_name = %q, want %q", got, want)
	}
}
