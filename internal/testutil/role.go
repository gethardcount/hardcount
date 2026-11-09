package testutil

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strings"
	"testing"
)

// CreateRole creates a login role for one test and returns its name. It
// works as the owner role in TEST_ADMIN_DATABASE_URL, on that database. The
// name is prefix plus the database's name, and the role is dropped, with
// its grants there, when the test ends. prefix holds only lowercase
// letters, digits and underscores. options are extra CREATE ROLE options,
// such as "BYPASSRLS", or "".
//
// A role belongs to the whole Postgres server, not to one database, so a
// fixed name is shared by every database on the server. Two runs of the
// same test on two test databases at once would then drop or change each
// other's role, and a grant one run leaves in its database stops the other
// from dropping the role. A name per database keeps each run's role its own.
func CreateRole(t *testing.T, prefix, password, options string) string {
	t.Helper()
	// Use a pool of the helper's own: AdminPool closes it after the cleanup
	// below has dropped the role. A pool the caller closes with a defer is
	// already closed when a t.Cleanup runs.
	admin := AdminPool(t)
	var database string
	if err := admin.QueryRowContext(t.Context(), `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatalf("read current database: %v", err)
	}
	role := roleName(prefix, database)

	// A run of this test on this database that stopped before its cleanup
	// left the role behind, with its grants. Drop it so CREATE ROLE works.
	if err := dropRole(admin, role); err != nil {
		t.Fatalf("drop leftover role %s: %v", role, err)
	}
	q := "CREATE ROLE " + role + " WITH LOGIN " + options +
		" PASSWORD '" + strings.ReplaceAll(password, "'", "''") + "'"
	if _, err := admin.ExecContext(t.Context(), q); err != nil {
		t.Fatalf("create role %s: %v", role, err)
	}
	t.Cleanup(func() {
		if err := dropRole(admin, role); err != nil {
			t.Errorf("drop role %s: %v", role, err)
		}
	})
	return role
}

// dropRole drops role and everything it owns or was granted in admin's
// database. DROP ROLE fails while any database still holds a grant to the
// role, and DROP OWNED BY clears only the database it runs in. A role from
// CreateRole is named after its database, so this one database is enough.
func dropRole(admin *sql.DB, role string) error {
	var exists bool
	if err := admin.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, role).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if _, err := admin.Exec("DROP OWNED BY " + role); err != nil {
		return err
	}
	_, err := admin.Exec("DROP ROLE " + role)
	return err
}

// roleName returns prefix, the database name and a hash of it, joined by
// underscores. The name keeps only lowercase letters, digits and
// underscores, so it needs no quoting in SQL. It fits Postgres's 63-byte
// limit on names, which would otherwise cut a long name short without an
// error. The hash keeps two databases apart when the readable part cannot,
// for example after a long name is cut.
func roleName(prefix, database string) string {
	sum := sha256.Sum256([]byte(database))
	suffix := "_" + hex.EncodeToString(sum[:4])
	var b strings.Builder
	for _, r := range strings.ToLower(database) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	readable := b.String()
	const maxName = 63
	if room := maxName - len(prefix) - 1 - len(suffix); len(readable) > room {
		readable = readable[:max(room, 0)]
	}
	return prefix + "_" + readable + suffix
}
