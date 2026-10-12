package postgres_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestIsTransient_TerminatedBackend settles, against a real server, which Go
// error a lost connection surfaces as. The unit table constructs PgErrors and
// driver errors by hand; this proves the driver actually returns one of them.
//
// It opens a dedicated connection, reads that connection's own backend pid,
// terminates only that pid, and asserts the next statement's error is
// transient. It touches no other session. Behind a transaction-mode pooler the
// first case skips (the pooler hides the loss from a statement outside a
// transaction); the in-transaction case runs on both paths.
func TestIsTransient_TerminatedBackend(t *testing.T) {
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	terminate := func(t *testing.T, pid int) {
		t.Helper()
		var ok bool
		if err := admin.QueryRowContext(ctx, `SELECT pg_terminate_backend($1)`, pid).Scan(&ok); err != nil || !ok {
			t.Fatalf("terminate backend %d: ok=%v err=%v", pid, ok, err)
		}
		// The signal is asynchronous: wait until the backend is gone.
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var alive bool
			if err := admin.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1)`, pid).Scan(&alive); err != nil {
				t.Fatalf("poll backend: %v", err)
			}
			if !alive {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("backend %d still alive after terminate", pid)
	}

	t.Run("statement on a dead connection", func(t *testing.T) {
		conn, err := db.Pool.Conn(ctx)
		if err != nil {
			t.Fatalf("conn: %v", err)
		}
		defer func() { _ = conn.Close() }()
		var pid int
		if err := conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatalf("pid: %v", err)
		}
		terminate(t, pid)
		var after int
		err = conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&after)
		if err == nil {
			if after != pid {
				// A transaction-mode pooler gave the next statement another
				// backend, so this client never saw the loss. The pooled
				// path is covered by the in-transaction case below, where
				// the pooler keeps the client on the terminated backend.
				t.Skipf("statement was re-routed from backend %d to %d by a connection pooler", pid, after)
			}
			t.Fatal("a statement on a terminated backend succeeded")
		}
		t.Logf("surfaced as %T: %v", err, err)
		if !postgres.IsTransient(err) {
			t.Fatalf("lost-connection error is not classed transient: %T %v", err, err)
		}
	})

	// The billing shape: the connection dies between two statements of one
	// open transaction.
	t.Run("statement inside an open transaction", func(t *testing.T) {
		conn, err := db.Pool.Conn(ctx)
		if err != nil {
			t.Fatalf("conn: %v", err)
		}
		defer func() { _ = conn.Close() }()
		tx, err := conn.BeginTx(ctx, &sql.TxOptions{})
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		var pid int
		if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatalf("pid: %v", err)
		}
		terminate(t, pid)
		_, err = tx.ExecContext(ctx, `SELECT 1`)
		if err == nil {
			t.Fatal("a statement on a terminated backend succeeded")
		}
		t.Logf("surfaced as %T: %v", err, err)
		if !postgres.IsTransient(err) {
			t.Fatalf("lost-connection error inside a transaction is not classed transient: %T %v", err, err)
		}
	})
}
