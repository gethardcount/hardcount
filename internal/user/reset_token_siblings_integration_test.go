package user_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/user"
)

// TestResetPassword_VoidsSiblingTokens: redeeming one password-reset token
// voids every other unused token of that user, in the same transaction.
// Anyone who knows the email can request a reset, so one account can hold
// several live tokens; an earlier link must not reset the password again
// after the owner has reset it.
//
// Real Postgres because the guarantee is one SQL statement's WHERE clause.
// The control: a different user's live token survives, or every concurrent
// reset in the tenant would be cancelled.
func TestResetPassword_VoidsSiblingTokens(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Reset Siblings")
	store := user.NewPostgresStore(db)
	victim := mkResetUser(t, db, tenantID, "victim@reset.test")
	bystander := mkResetUser(t, db, tenantID, "bystander@reset.test")

	issue := func(userID, hash string) {
		t.Helper()
		if _, err := store.CreateResetToken(ctx, userID, hash, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("issue token %s: %v", hash, err)
		}
	}
	// Three live tokens on the victim and one on an unrelated user.
	issue(victim, "hash_redeemed")
	issue(victim, "hash_sibling_a")
	issue(victim, "hash_sibling_b")
	issue(bystander, "hash_bystander")

	redeem := func(hash string) error {
		_, err := store.ResetPassword(ctx, hash, "$2a$12$unused-hash-for-this-test", func(*sql.Tx, string) error { return nil })
		return err
	}
	if err := redeem("hash_redeemed"); err != nil {
		t.Fatalf("redeem: %v", err)
	}

	// Proven by trying to redeem them: redemption is what a second holder
	// of a link needs.
	for _, h := range []string{"hash_sibling_a", "hash_sibling_b"} {
		if err := redeem(h); !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("redeem sibling token %s after another token reset the password: err = %v, want errs.ErrNotFound", h, err)
		}
	}
	if err := redeem("hash_bystander"); err != nil {
		t.Errorf("another user's live token was voided (%v); the void must be scoped to the redeeming user", err)
	}
}
