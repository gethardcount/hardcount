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
// Postgres stopped it to break a deadlock (DeadlockAsConflict).
const CodeConcurrentChange = "concurrent_change"

// concurrentChangeMessage is the 409's message. The dashboard shows it to an
// operator as it is, so it names no cause, no SQL and no idempotency key.
const concurrentChangeMessage = "Another change to the same records ran at the same time, so this request stopped before it finished. Send it again."

// DeadlockAsConflict returns a 409 concurrent_change in place of err when
// Postgres aborted the request's transaction to break a deadlock (SQLSTATE
// 40P01). Any other error, nil included, comes back unchanged. The
// idempotency layer does not store a 409, so the client can send the same
// request again with the same key. The raw error and the deadlock's detail
// go to the log at WARN with the route, because the answer carries neither.
//
// Call it only in a handler, and only where everything the request committed
// before the transaction that lost is safe to run twice (ADR-136 lists the
// routes). Never call it in a service or a store: callers there read
// errs.ErrInvalidState as a harmless outcome, such as a webhook already
// handled, so a deadlock turned into one would be dropped instead of run
// again.
func DeadlockAsConflict(r *http.Request, err error) error {
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
	return errs.InvalidState(concurrentChangeMessage).WithCode(CodeConcurrentChange)
}
