package respond

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// CodeConcurrentChange is the code of the 409 a request answers when
// Postgres stopped it to break a deadlock, or ended its wait for a lock
// (ConcurrentChangeAsConflict), and when a cancel's subscription changed
// twice while the cancel priced it (ConcurrentChange, ADR-139).
const CodeConcurrentChange = "concurrent_change"

// concurrentChangeMessage is the 409's message. The dashboard shows it to an
// operator as it is, so it names no cause, no SQL and no idempotency key.
const concurrentChangeMessage = "Another change to the same records ran at the same time, so this request stopped before it finished. Send it again."

// ConcurrentChange is the 409 concurrent_change itself. A handler returns it
// for a change that stopped, with nothing written, because another change to
// the same records committed while it ran: the cancel whose subscription
// changed twice while it was priced (ADR-139). The idempotency layer does not
// store a 409, so the client can send the same request again with the same
// key. ConcurrentChangeAsConflict answers it for a deadlock or an ended lock
// wait.
func ConcurrentChange() error {
	return errs.InvalidState(concurrentChangeMessage).WithCode(CodeConcurrentChange)
}

// ConcurrentChangeAsConflict returns a 409 concurrent_change in place of err
// when Postgres aborted the request's transaction to break a deadlock
// (SQLSTATE 40P01), or ended a statement that waited longer than
// lock_timeout for a lock another transaction held (SQLSTATE 55P03). Any
// other error, nil included, comes back unchanged; a statement timeout
// (57014) is not converted. The idempotency layer does not store a 409, so
// the client can send the same request again with the same key. The raw
// error goes to the log at WARN with the route, on a line that names which
// of the two it was, because the answer carries neither.
//
// Call it only in a handler, and only where everything the request committed
// before the transaction that failed is safe to run twice (ADR-136 lists the
// routes). Never call it in a service or a store: callers there read
// errs.ErrInvalidState as a harmless outcome, such as a webhook already
// handled, so a deadlock turned into one would be dropped instead of run
// again.
func ConcurrentChangeAsConflict(r *http.Request, err error) error {
	if postgres.IsLockTimeout(err) {
		slog.WarnContext(r.Context(), "request's lock wait ended at the lock limit; answering 409",
			"error", err,
			"method", r.Method,
			"route", routePattern(r),
		)
		return ConcurrentChange()
	}
	if !postgres.IsDeadlock(err) {
		return err
	}
	var detail string
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		detail = pgErr.Detail
	}
	slog.WarnContext(r.Context(), "request lost a database deadlock; answering 409",
		"error", err,
		"detail", detail,
		"method", r.Method,
		"route", routePattern(r),
	)
	return ConcurrentChange()
}
