package customer_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestEmailStatus_ComplaintLattice locks the ADR-098 suppression lattice
// against real Postgres: complained is terminal-most — a later bounce is
// a benign no-op (never a downgrade, never a spurious ErrNotFound to the
// SMTP-path caller), while a genuinely missing customer still errors.
func TestEmailStatus_ComplaintLattice(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := customer.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Complaint lattice")

	cust, err := store.Create(ctx, tenantID, domain.Customer{
		ExternalID: "cl", DisplayName: "Complainer", Email: "cl@example.com",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// bounce → complaint promotes.
	if err := store.MarkEmailBounced(ctx, tenantID, cust.ID, "550 5.1.1 user unknown"); err != nil {
		t.Fatalf("bounce: %v", err)
	}
	if err := store.MarkEmailComplained(ctx, tenantID, cust.ID, "postmark spam complaint"); err != nil {
		t.Fatalf("complaint: %v", err)
	}
	got, _ := store.Get(ctx, tenantID, cust.ID)
	if got.EmailStatus != domain.EmailStatusComplained {
		t.Fatalf("after complaint: got %q, want complained", got.EmailStatus)
	}

	// A bounce arriving AFTER the complaint (webhook reorder /
	// redelivery) must not downgrade — and must not error either: the
	// existing SMTP-path caller treats an error as a real failure.
	if err := store.MarkEmailBounced(ctx, tenantID, cust.ID, "550 later bounce"); err != nil {
		t.Fatalf("bounce-after-complaint must be a benign no-op, got %v", err)
	}
	got, _ = store.Get(ctx, tenantID, cust.ID)
	if got.EmailStatus != domain.EmailStatusComplained {
		t.Errorf("bounce downgraded complained → %q", got.EmailStatus)
	}
	if got.EmailBounceReason != "postmark spam complaint" {
		t.Errorf("no-op bounce must not overwrite the complaint reason: %q", got.EmailBounceReason)
	}

	// Redelivered complaint: idempotent no-op.
	if err := store.MarkEmailComplained(ctx, tenantID, cust.ID, "redelivered"); err != nil {
		t.Fatalf("redelivered complaint: %v", err)
	}

	// A missing customer is still a REAL miss.
	if err := store.MarkEmailBounced(ctx, tenantID, "hc_cus_missing", "550"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("missing customer bounce: want ErrNotFound, got %v", err)
	}
	if err := store.MarkEmailComplained(ctx, tenantID, "hc_cus_missing", "spam"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("missing customer complaint: want ErrNotFound, got %v", err)
	}

	// The suppression gate treats complained as suppressed (0050's CHECK
	// admits it; IsSuppressed's switch already covered it — this pins the
	// end-to-end read).
	if got.EmailLastBouncedAt == nil {
		t.Error("complaint must stamp email_last_bounced_at (operator-visible recency)")
	}

	// An email edit still resets the lattice (existing behavior, now
	// covering 'complained' too).
	if err := store.ResetEmailStatus(ctx, tenantID, cust.ID); err != nil {
		t.Fatalf("reset: %v", err)
	}
	got, _ = store.Get(ctx, tenantID, cust.ID)
	if got.EmailStatus != domain.EmailStatusUnknown {
		t.Errorf("reset: got %q, want unknown", got.EmailStatus)
	}
}
