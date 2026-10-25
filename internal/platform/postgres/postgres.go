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
	DefaultQueryTimeout     = 5 * time.Second
	DefaultMigrationTimeout = 60 * time.Second
)

// DB wraps *sql.DB with query timeout and RLS-aware transactions.
type DB struct {
	Pool         *sql.DB
	QueryTimeout time.Duration
}

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

// Context key for livemode propagation. Owned by the postgres package because
// BeginTx is the single choke point that needs to read it — having the key
// live here avoids circular imports between auth and postgres.
// The value is stored as *bool so an explicit false can be told apart from
// "never set".
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
// app.livemode is unset (migrations 00212 and 00213).
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

func Rollback(tx *sql.Tx) {
	if tx != nil {
		_ = tx.Rollback()
	}
}

// WithTenantTx runs fn inside one tenant-scoped (TxTenant) transaction:
// commit when fn returns nil, rollback otherwise. It exists as the seam for
// cross-store coordinator transactions — a caller that must commit writes in
// two domain stores atomically (e.g. the billing engine's threshold
// fire + cycle re-anchor) passes the same *sql.Tx to each store's Tx
// variant. Unit tests fake the interface this satisfies with a runner that
// calls fn(nil); the store mocks ignore the tx handle.
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

// NewID generates a prefixed, time-sortable ID (e.g., vlx_cus_cv2q6ktjml6ng3v2q0tg).
// Uses xid: globally unique, 20-char, URL-safe, naturally ordered by creation time.
func NewID(prefix string) string {
	return fmt.Sprintf("%s_%s", prefix, xid.New().String())
}

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
// unique-violation (SQLSTATE 23505), otherwise "". Use this to disambiguate
// multiple unique constraints on the same table — e.g. subscriptions has
// both (tenant_id, code) and a partial-unique (tenant_id, customer_id, plan_id)
// for live statuses, and the callers need to surface distinct errors.
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
// violation (SQLSTATE 23514). Used to translate DB-level invariant failures
// (e.g. the test_clocks livemode CHECK, or subscriptions_test_clock_requires_testmode)
// into user-facing 400s instead of leaking raw SQL error text.
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

// IsTransient reports whether err means the statement never reached a verdict
// because the connection or the transaction was taken away: the connection
// was lost, the server is shutting down, or the server aborted the transaction
// to resolve a serialization conflict or a deadlock. The row a statement was
// working on cannot cause any of these, so a caller that counts failures
// against a row (the billing run, ADR-119) does not count one.
//
// A timeout the server or the caller decided is deliberately NOT transient:
// not a statement timeout (57014), not a lock timeout (55P03), and not a Go
// deadline. Such a timeout is the only way a query that is too slow for its
// own data ever shows up, and it recurs. Resource exhaustion (class 53) is not
// transient either.
//
// A network timeout with no ctx error behind it IS transient: the kernel gave
// up on a connection to a dead or partitioned server (ETIMEDOUT), or a dial
// timed out. That is a lost connection that never got a reset. The driver
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

func NullableString(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}

// NullableStringPtr converts a *string into a sql-friendly value: nil
// (becomes SQL NULL) when the pointer is nil or points at an empty
// string, otherwise the dereferenced value. Saves callers from juggling
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

func NullableTime(v *time.Time) any {
	if v == nil {
		return nil
	}
	return *v
}

// StringArray is a []string that implements sql.Scanner and driver.Valuer
// for PostgreSQL TEXT[] columns.
type StringArray []string

// Value converts the StringArray to a PostgreSQL array literal.
//
// Observed with the pgx stdlib driver: a nil StringArray reaches Postgres as
// NULL, not '{}', despite the nil branch below. For a negated test that
// matters: NOT (x = ANY(NULL)) is NULL and filters out every row. COALESCE
// the parameter in SQL when nil can reach a NOT ANY / <> ALL
// (TestDueScans_ExcludeAppliesBeforeLimit fails without it).
func (a StringArray) Value() (interface{}, error) {
	if a == nil {
		return "{}", nil
	}
	if len(a) == 0 {
		return "{}", nil
	}
	elements := make([]string, len(a))
	for i, s := range a {
		// Escape double quotes and backslashes inside elements
		escaped := strings.ReplaceAll(s, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		elements[i] = `"` + escaped + `"`
	}
	return "{" + strings.Join(elements, ",") + "}", nil
}

// Scan parses a PostgreSQL TEXT[] value into a StringArray.
func (a *StringArray) Scan(src interface{}) error {
	if src == nil {
		*a = StringArray{}
		return nil
	}
	var s string
	switch v := src.(type) {
	case string:
		s = v
	case []byte:
		s = string(v)
	default:
		return fmt.Errorf("StringArray.Scan: unsupported type %T", src)
	}
	// Trim outer braces
	s = strings.TrimSpace(s)
	if s == "{}" || s == "" {
		*a = StringArray{}
		return nil
	}
	s = s[1 : len(s)-1] // Remove { and }
	var result []string
	var current strings.Builder
	inQuote := false
	escaped := false
	for _, ch := range s {
		if escaped {
			current.WriteRune(ch)
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == '"' {
			inQuote = !inQuote
			continue
		}
		if ch == ',' && !inQuote {
			result = append(result, current.String())
			current.Reset()
			continue
		}
		current.WriteRune(ch)
	}
	if current.Len() > 0 {
		result = append(result, current.String())
	}
	*a = result
	return nil
}
