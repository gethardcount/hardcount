// Package timefilter parses the ?from and ?to query parameters, and named
// variants such as ?date_from and ?date_to, for the operator-facing list
// and export endpoints. Every endpoint shares one contract, so an operator
// who learns one endpoint can use the next one the same way.
//
// It accepts two formats:
//   - RFC3339 instants, such as "2026-01-01T00:00:00Z". The dashboard sends
//     these, derived from the start and end of day in the tenant's time
//     zone (ADR-010).
//   - Bare YYYY-MM-DD dates, such as "2026-01-01". These are convenient for
//     curl and CLI users running ad-hoc queries.
//
// A date-only value is anchored at UTC. `from` becomes 00:00:00Z, the
// start of the day. `to` becomes 23:59:59.999999Z, the last microsecond of
// the day, so the day named in `to` is included. That matches what an
// operator typing "from=2026-01-01&to=2026-01-31" expects.
//
// The end is the last microsecond, not the last second. Postgres
// timestamptz keeps microseconds, so a 23:59:59Z bound would drop every row
// stamped 23:59:59.000001 to .999999. It is not .999999999 either, because
// Postgres cannot hold nanoseconds. Whether such a value is truncated or
// rounded up to the next day's 00:00:00 depends on how it reaches the
// server.
//
// Consumers do not all compare `to` the same way:
//   - Inclusive (`<= to`): the invoice list and invoices.csv,
//     customers.csv, subscriptions.csv, usage-events.csv, the audit log and
//     audit-log.csv (both created_at and sim_effective_at).
//   - Exclusive (`< to`): the usage event list and aggregate
//     (/v1/usage-events, /v1/usage-events/aggregate), all through
//     usage.buildUsageWhere, and customer usage (/v1/customers/{id}/usage,
//     through usage.AggregateClaims).
//
// For a date-only `to`, both read the same in practice. The exclusive
// consumers miss only a row stamped at exactly 23:59:59.999999.
package timefilter

import (
	"net/http"
	"time"

	"github.com/gethardcount/hardcount/internal/errs"
)

// ParseRange reads two named query parameters from the request and returns
// their parsed time.Time values. A missing parameter returns the zero time,
// and callers branch on IsZero to skip that side of the filter.
//
// It returns errs.Invalid on malformed input. The handler should pass that
// to respond.FromError, which answers 422 validation_error naming the
// parameter. It also refuses an inverted range (`to` before `from`). Equal
// endpoints are allowed (see below).
//
// fromParam and toParam are usually "from" and "to", but the audit log uses
// the legacy names "date_from" and "date_to". Pass the names the endpoint
// already exposes. This helper does not normalize the names, because that
// would change a URL contract that operators see.
func ParseRange(r *http.Request, fromParam, toParam string) (from, to time.Time, err error) {
	if v := r.URL.Query().Get(fromParam); v != "" {
		from, err = parseOne(v, false)
		if err != nil {
			return time.Time{}, time.Time{}, errs.Invalid(fromParam, "must be RFC3339 (e.g. 2026-01-01T00:00:00Z) or YYYY-MM-DD")
		}
	}
	if v := r.URL.Query().Get(toParam); v != "" {
		to, err = parseOne(v, true)
		if err != nil {
			return time.Time{}, time.Time{}, errs.Invalid(toParam, "must be RFC3339 (e.g. 2026-12-31T23:59:59Z) or YYYY-MM-DD")
		}
	}
	// Refuse only an inverted range. Equal endpoints are a valid zero-width
	// window for the inclusive consumers (>= and <=, listed in the package
	// doc), so from==to selects the rows at exactly that instant. That is
	// the natural query on the audit log's simulated-time axis, where
	// sim_effective_at falls on advance targets and cycle boundaries. The
	// exclusive consumers apply their own check: customer usage refuses
	// from==to, and the usage event list and aggregate return nothing.
	if !from.IsZero() && !to.IsZero() && to.Before(from) {
		return time.Time{}, time.Time{}, errs.Invalid(toParam, "must not be before `"+fromParam+"`")
	}
	return from, to, nil
}

// parseOne tries RFC3339 first (the canonical wire format), then a bare
// YYYY-MM-DD as the operator-friendly fallback. endOfDay=true anchors a
// date-only value at 23:59:59.999999Z instead of 00:00:00Z. The `to` side
// of the range uses it, so the day named in the filter is included.
func parseOne(s string, endOfDay bool) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		if endOfDay {
			return time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, endOfDayNanos, time.UTC), nil
		}
		return t.UTC(), nil
	}
	return time.Time{}, errInvalidFormat
}

// endOfDayNanos is the last microsecond of a second, in nanoseconds.
// Postgres timestamptz stores microseconds, so this is the latest
// instant of a day it can hold.
const endOfDayNanos = 999_999_000

// errInvalidFormat is what parseOne returns for malformed input. Its
// caller, ParseRange, wraps it in an errs.Invalid that carries the
// parameter name and a format hint, so the 422 response says which field
// to fix.
var errInvalidFormat = invalidFormatErr{}

type invalidFormatErr struct{}

func (invalidFormatErr) Error() string { return "invalid date format" }
