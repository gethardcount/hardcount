package user

import (
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/session"
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

// issueTestSession creates a session on its own bypass transaction and returns
// its cookie value.
func issueTestSession(t *testing.T, db *postgres.DB, svc *session.Service, in session.IssueInput) string {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	raw, _, err := svc.IssueTx(t.Context(), tx, in)
	if err != nil {
		t.Fatalf("issue a session for %s in %s (live=%v): %v", in.UserID, in.TenantID, in.Livemode, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return raw
}
