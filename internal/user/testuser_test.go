package user

import (
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// CreateTestUser adds an account that belongs to tenantID with role, the
// way bootstrap does: HashPassword, then CreateInTx on one transaction. The
// real-Postgres tests in this package and in package user_test use it to set
// up an account to sign in to or reset.
func CreateTestUser(t *testing.T, db *postgres.DB, email, password, tenantID, role string) domain.User {
	t.Helper()
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("hash the password for %s: %v", email, err)
	}
	tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin tx for %s: %v", email, err)
	}
	defer postgres.Rollback(tx)
	u, err := NewPostgresStore(db).CreateInTx(t.Context(), tx, email, hash, tenantID, role)
	if err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit user %s: %v", email, err)
	}
	return u
}
