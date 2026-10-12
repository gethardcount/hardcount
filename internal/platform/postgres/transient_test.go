package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

type fakeNetErr struct{ timeout bool }

func (e fakeNetErr) Error() string   { return "fake net error" }
func (e fakeNetErr) Timeout() bool   { return e.timeout }
func (e fakeNetErr) Temporary() bool { return false }

// TestIsTransient pins the class the billing run does not count against a
// subscription (ADR-119): the connection or the transaction was taken away.
// Timeouts of every kind, constraint violations and resource errors are
// verdicts about the work and are not transient.
func TestIsTransient(t *testing.T) {
	pg := func(code string) error { return fmt.Errorf("exec: %w", &pgconn.PgError{Code: code}) }
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"08000 connection exception", pg("08000"), true},
		{"08003 connection does not exist", pg("08003"), true},
		{"08006 connection failure", pg("08006"), true},
		{"08P01 protocol violation", pg("08P01"), true},
		{"57P01 admin shutdown", pg("57P01"), true},
		{"57P02 crash shutdown", pg("57P02"), true},
		{"57P03 cannot connect now", pg("57P03"), true},
		{"40001 serialization failure", pg("40001"), true},
		{"40P01 deadlock detected", pg("40P01"), true},

		{"23505 unique violation", pg("23505"), false},
		{"23514 check violation", pg("23514"), false},
		{"57014 statement timeout / query canceled", pg("57014"), false},
		{"55P03 lock not available", pg("55P03"), false},
		{"53100 disk full", pg("53100"), false},
		{"53300 too many connections", pg("53300"), false},
		{"40002 integrity constraint at commit", pg("40002"), false},
		{"25P02 in failed transaction", pg("25P02"), false},

		{"driver.ErrBadConn", fmt.Errorf("begin: %w", driver.ErrBadConn), true},
		{"sql.ErrConnDone", fmt.Errorf("query: %w", sql.ErrConnDone), true},
		{"io.EOF", fmt.Errorf("read: %w", io.EOF), true},
		{"io.ErrUnexpectedEOF", fmt.Errorf("read: %w", io.ErrUnexpectedEOF), true},
		{"net error, not a timeout", fmt.Errorf("write: %w", fakeNetErr{}), true},
		{"*net.OpError", &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}, true},

		// A kernel or dial timeout: the connection was lost without a reset
		// (a dead or partitioned server). No ctx error is in the chain.
		{"net error that is a timeout", fmt.Errorf("write: %w", fakeNetErr{timeout: true}), true},
		{"context.DeadlineExceeded", fmt.Errorf("query: %w", context.DeadlineExceeded), false},
		{"context.Canceled", fmt.Errorf("query: %w", context.Canceled), false},
		{"net error wrapping a deadline", fmt.Errorf("%w: %w", fakeNetErr{}, context.DeadlineExceeded), false},
		{"plain error", errors.New("plan lookup refused"), false},
		{"sql.ErrNoRows", sql.ErrNoRows, false},
		{"sql.ErrTxDone", sql.ErrTxDone, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsTransient(c.err); got != c.want {
				t.Fatalf("IsTransient(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
