// Package postgres opens database transactions with the settings that
// row-level security (RLS) reads. It also holds small helpers for IDs,
// Postgres errors and nullable values.
package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rs/xid"
)

const (
	// DefaultQueryTimeout is the QueryTimeout that NewDB uses when it is
	// given zero or less.
	DefaultQueryTimeout     = 5 * time.Second
	DefaultMigrationTimeout = 60 * time.Second
)

// DB wraps *sql.DB with a query timeout and RLS-aware transactions.
type DB struct {
	Pool         *sql.DB
	QueryTimeout time.Duration
}

// NewDB returns a DB on pool. A queryTimeout of zero or less means
// DefaultQueryTimeout.
func NewDB(pool *sql.DB, queryTimeout time.Duration) *DB {
	if queryTimeout <= 0 {
		queryTimeout = DefaultQueryTimeout
	}
	return &DB{Pool: pool, QueryTimeout: queryTimeout}
}

// TxMode controls how a transaction sets RLS session variables.
type TxMode int

const (
	TxTenant TxMode = iota // Sets app.tenant_id, enforces RLS
	TxBypass               // Sets app.bypass_rls = on, skips RLS
)

// livemodeCtxKey is the ctx key that carries livemode. The postgres package
// owns it because BeginTx is the one place that must read it. Keeping the
// key here also avoids an import cycle between auth and postgres. The value
// is a *bool, so an explicit false can be told apart from "never set".
type livemodeCtxKey struct{}

// WithLivemode returns a derived context carrying the mode flag. Every entry
// point sets it: the auth middleware from the key or session, public links
// from their token's row, and background workers per row or per mode. BeginTx
// reads it to set app.livemode, which the RLS policy filters on alongside
// tenant.
func WithLivemode(ctx context.Context, live bool) context.Context {
	return context.WithValue(ctx, livemodeCtxKey{}, &live)
}

// ErrLivemodeUnset means ctx carries no livemode. Every entry point sets one
// with WithLivemode, so this is a bug at the entry point. Nothing guesses a
// mode in its place: the database refuses an insert, and shows no row, when
// app.livemode is unset (migrations 00213 and 00214).
var ErrLivemodeUnset = errors.New("postgres: livemode was never set on ctx (set postgres.WithLivemode at the entry point)")

// ErrTenantUnset means a tenant-scoped transaction was asked for with no
// tenant ID.
var ErrTenantUnset = errors.New("postgres: TxTenant opened with no tenant ID")

// Livemode reads the livemode flag from ctx, or returns ErrLivemodeUnset.
func Livemode(ctx context.Context) (bool, error) {
	p, ok := ctx.Value(livemodeCtxKey{}).(*bool)
	if !ok || p == nil {
		return false, ErrLivemodeUnset
	}
	return *p, nil
}

// BeginTx opens a transaction and sets the RLS settings for mode, local to
// the transaction. TxTenant sets app.tenant_id from tenantID and
// app.livemode from ctx (WithLivemode). A blank tenantID returns
// ErrTenantUnset, and a ctx with no livemode returns ErrLivemodeUnset, before
// anything opens. TxBypass turns
// app.bypass_rls on, so RLS does not apply and the caller must filter by
// tenant itself.
func (db *DB) BeginTx(ctx context.Context, mode TxMode, tenantID string) (*sql.Tx, error) {
	var tid, lm string
	if mode == TxTenant {
		tid = strings.TrimSpace(tenantID)
		if tid == "" {
			return nil, ErrTenantUnset
		}
		live, err := Livemode(ctx)
		if err != nil {
			return nil, err
		}
		lm = "off"
		if live {
			lm = "on"
		}
	}

	tx, err := db.Pool.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}

	switch mode {
	case TxTenant:
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.bypass_rls', 'off', true)`); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tid); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', $1, true)`, lm); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
	case TxBypass:
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.bypass_rls', 'on', true)`); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
	}

	return tx, nil
}

// Rollback rolls tx back and ignores the error. A nil tx does nothing.
func Rollback(tx *sql.Tx) {
	if tx != nil {
		_ = tx.Rollback()
	}
}

// WithTenantTx runs fn inside one tenant-scoped (TxTenant) transaction. It
// commits when fn returns nil and rolls back otherwise. It is the seam for
// coordinator transactions that span stores. A caller that must commit
// writes in two domain stores atomically passes the same *sql.Tx to each
// store's Tx variant, as the billing engine's threshold fire and cycle
// re-anchor do. Unit tests fake the interface this satisfies with a runner
// that calls fn(nil), and the store mocks ignore the tx handle.
func (db *DB) WithTenantTx(ctx context.Context, tenantID string, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer Rollback(tx)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// NewID generates a prefixed, time-sortable ID such as
// hc_cus_cv2q6ktjml6ng3v2q0tg. It uses xid, which is globally unique, 20
// characters long, URL-safe and naturally ordered by creation time.
func NewID(prefix string) string {
	return fmt.Sprintf("%s_%s", prefix, xid.New().String())
}

// IsUniqueViolation reports whether err is a Postgres unique violation
// (SQLSTATE 23505).
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

// IsDeadlock reports whether err is Postgres aborting the transaction to
// break a deadlock (SQLSTATE 40P01). Nothing the transaction wrote is kept,
// so running it again from the start is safe.
func IsDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

// UniqueViolationConstraint returns the constraint name if err is a Postgres
// unique violation (SQLSTATE 23505), and "" otherwise. Use it to tell apart
// several unique constraints on one table. For example, subscriptions has
// both (tenant_id, code) and a partial unique (tenant_id, customer_id,
// plan_id) for live statuses, and the callers need to return distinct
// errors.
func UniqueViolationConstraint(err error) string {
	if err == nil {
		return ""
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName
	}
	return ""
}

// IsForeignKeyViolation reports whether err is a Postgres foreign-key
// violation (SQLSTATE 23503).
func IsForeignKeyViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23503"
	}
	return false
}

// IsCheckViolation reports whether err is a Postgres check-constraint
// violation (SQLSTATE 23514). Callers use it to turn a database invariant
// failure into a 400 for the user, instead of leaking raw SQL error text.
// Examples are the test_clocks livemode CHECK and
// subscriptions_test_clock_requires_testmode.
func IsCheckViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23514"
	}
	return false
}

// IsTransient reports whether err means the statement never reached a
// verdict because the connection or the transaction was taken away. That
// covers a lost connection, a server shutting down, and a transaction the
// server aborted to resolve a serialization conflict or a deadlock. The row
// a statement was working on cannot cause any of these. So a caller that
// counts failures against a row (the billing run, ADR-119) does not count
// one.
//
// A timeout that the server or the caller decided is not transient, on
// purpose: a statement timeout (57014), a lock timeout (55P03) or a Go
// deadline. Such a timeout is the only way a query that is too slow for its
// own data ever shows up, and it recurs. Resource exhaustion (class 53) is
// not transient either.
//
// A network timeout with no ctx error behind it is transient. The kernel
// gave up on a connection to a dead or partitioned server (ETIMEDOUT), or a
// dial timed out: a lost connection that never got a reset. The driver
// reports a Go deadline as a wrapped context error, which the first check
// below has already turned away, so the two cannot be confused by type.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	// A cancelled or expired ctx surfaces through the driver in many shapes,
	// some of them net errors. Whether that is an interruption is the
	// caller's question about its own ctx, not a property of the connection.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "57P01", "57P02", "57P03", // admin shutdown, crash shutdown, cannot connect now
			"40001", "40P01": // serialization failure, deadlock detected
			return true
		}
		return strings.HasPrefix(pgErr.Code, "08") // connection exception
	}
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// NullableString returns nil, which becomes SQL NULL, when v is empty or
// only white space, and v otherwise.
func NullableString(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}

// NullableStringPtr converts a *string into a value for a SQL argument. It
// is nil, which becomes SQL NULL, when the pointer is nil or points at an
// empty string, and the dereferenced value otherwise. Callers then need no
// sql.NullString for optional reference columns.
func NullableStringPtr(v *string) any {
	if v == nil {
		return nil
	}
	if strings.TrimSpace(*v) == "" {
		return nil
	}
	return *v
}

// NullableTime returns nil, which becomes SQL NULL, for a nil pointer, and
// the time otherwise.
func NullableTime(v *time.Time) any {
	if v == nil {
		return nil
	}
	return *v
}
