// Package timefilter is the shared parser for ?from / ?to (and named
// variants like ?date_from / ?date_to) query parameters across
// operator-facing list and export endpoints. One contract everywhere
// so an operator who learns one endpoint doesn't trip on the next.
//
// Accepts two formats:
//   - RFC3339 instants: "2026-01-01T00:00:00Z" — what the dashboard
//     sends, derived from tenant-TZ start/end-of-day per ADR-010.
//   - Bare YYYY-MM-DD dates: "2026-01-01" — convenient for curl/CLI
//     users running ad-hoc queries.
//
// Date-only inputs are anchored at UTC: `from` becomes 00:00:00Z (start
// of day), `to` becomes 23:59:59.999999Z (the last microsecond of the
// day) so the day named in `to` is included — matches what an operator
// typing "from=2026-01-01&to=2026-01-31" intuitively expects. The end
// is the last MICROSECOND, not the last second: Postgres timestamptz
// keeps microseconds, so a 23:59:59Z bound dropped every row stamped
// 23:59:59.000001–.999999. It is not .999999999 either: Postgres cannot
// hold nanoseconds, and whether such a value is truncated or rounded up
// to the next day's 00:00:00 depends on how it reaches the server.
//
// Consumers do not all compare `to` the same way:
//   - inclusive (`<= to`): invoice list and invoices.csv, customers.csv,
//     subscriptions.csv, usage-events.csv, the audit log and
//     audit-log.csv (both created_at and sim_effective_at).
//   - exclusive (`< to`): the usage summary (/v1/usage-summary, via
//     usage.buildUsageWhere) and customer usage (/v1/customers/{id}/usage,
//     via usage.AggregateByPricingRules).
//
// For a date-only `to` both read the same in practice: the exclusive
// consumers miss only a row stamped at exactly 23:59:59.999999.
package timefilter

import (
	"net/http"
	"time"

	"github.com/sagarsuperuser/velox/internal/errs"
)

// ParseRange reads two named query params from the request and returns
// their parsed time.Time values. Missing params return zero-value times
// (callers branch on IsZero to skip the filter).
//
// Returns errs.Invalid on malformed input — the handler should pipe
// through respond.FromError, which answers 422 validation_error naming
// the param. Also rejects an inverted range (`to` before `from`);
// equal endpoints are allowed (see below).
//
// fromParam and toParam are usually "from"/"to" but the audit log uses
// the legacy "date_from"/"date_to" naming. Pass whichever the endpoint
// has historically exposed; this helper doesn't normalize the param
// names because that would be an operator-visible URL contract change.
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
	// Reject only INVERTED ranges. Equal endpoints are a valid zero-width
	// window for the inclusive consumers (>= / <=, listed in the package
	// doc), so from==to selects rows at exactly that instant — which is THE
	// natural query on the audit log's sim axis, where sim_effective_at is
	// quantized to advance targets and cycle boundaries (found on the FLOW
	// P2 walk: the flow's own single-instant example 422'd against the
	// stricter check). The exclusive consumers apply their own check:
	// customer usage rejects from==to; the usage summary returns nothing.
	if !from.IsZero() && !to.IsZero() && to.Before(from) {
		return time.Time{}, time.Time{}, errs.Invalid(toParam, "must not be before `"+fromParam+"`")
	}
	return from, to, nil
}

// parseOne tries RFC3339 first (the canonical wire format), then bare
// YYYY-MM-DD as the operator-friendly fallback. endOfDay=true anchors
// a date-only value at 23:59:59.999999Z instead of 00:00:00Z — used for
// the `to` side of the range so the day named in the filter is included.
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

// errInvalidFormat is returned by parseOne for malformed input. The
// caller (ParseRange) wraps it in an errs.Invalid with the param name
// + format hint so the eventual 422 response identifies which field
// to fix.
var errInvalidFormat = invalidFormatErr{}

type invalidFormatErr struct{}

func (invalidFormatErr) Error() string { return "invalid date format" }
