package errs

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// SafeMessageError is the opt-in marker for an error type that owns its
// operator-safe rendering (ADR-026).
//
// An error type implements it when it wraps a message that was categorized
// upstream and is already curated for the operator, such as a Stripe card
// decline with its decline_code or a classified tax provider failure. The
// type then bypasses the default generic sanitizing. SanitizeForOperator
// finds it with errors.As and returns OperatorSafeMessage() instead.
//
// Never implement it on an error that includes raw provider strings,
// database internals, panic stacks or anything else unsafe for an operator.
// Those are exactly what the boundary sanitizer exists to suppress.
//
// It lives in internal/errs so that code outside HTTP can use the same
// dispatch: the test-clock catch-up orchestrator, scheduler error rollups
// and audit-log writers. respond.SafeMessageError is an alias of it.
type SafeMessageError interface {
	error
	OperatorSafeMessage() string
}

// genericInternalError is the operator-grade fallback when an error has no
// SafeMessageError implementation and contains internal-only markers. The
// request_id lets support trace the full diagnostic in slog. The capital on
// the first word is intended, because this string lands in dashboard
// banners and toast bodies.
func genericInternalError(requestID string) string {
	if requestID == "" {
		return "An internal error occurred. See server logs for details."
	}
	return fmt.Sprintf("An internal error occurred. Reference: %s", requestID)
}

// SanitizeForOperator returns an operator-safe rendering of err, for any
// boundary where the error message lands in the dashboard, in a column the
// dashboard reads, or in an HTTP body (ADR-026).
//
// It works in this order:
//
//  1. If err, or any error in its chain, implements SafeMessageError,
//     return OperatorSafeMessage(). The error type opted into curated
//     rendering.
//  2. Otherwise, scan the message for internal-only markers: SQLSTATE
//     codes, Go file:line frames, panic markers and raw stack traces. If
//     any is present, return the generic "internal error reference:
//     REQ-XYZ" message. The full diagnostic stays in slog for support to
//     trace, which is the caller's job.
//  3. Otherwise, treat the message as operator-safe (Stripe text, domain
//     validation errors with curated copy and so on). Pass it through after
//     Scrub redacts personal data (PII).
//
// The third branch is conservative on purpose: there is no allow-list of
// known-safe patterns. The marker check in step 2 is the filter that does
// the work. Anything shaped like a SQLSTATE, a file:line or a panic is
// internal by construction. Tests pin the marker set.
func SanitizeForOperator(err error, requestID string) string {
	if err == nil {
		return ""
	}
	var sme SafeMessageError
	if errors.As(err, &sme) {
		return Scrub(sme.OperatorSafeMessage())
	}
	msg := err.Error()
	if hasInternalMarkers(msg) {
		return genericInternalError(requestID)
	}
	return Scrub(msg)
}

// SummarizeForOperator builds a short operator-grade summary of a batch of
// errors, usually the per-row errors of a multi-row catch-up phase or a
// scheduler tick. It returns "" when errs is empty.
//
// The output, by category:
//
//   - Each error that implements SafeMessageError is included verbatim
//     after Scrub, joined with "; ".
//   - All others are counted, and appear as "N internal error(s)" with the
//     request_id reference.
//
// The full diagnostic for every error stays in slog. That is the caller's
// job: SummarizeForOperator never logs.
//
// Examples:
//
//	SummarizeForOperator([err1_safe, err2_internal, err3_internal], "REQ-1")
//	  → "stripe tax: customer has no country on billing profile; 2 internal error(s). Reference: REQ-1"
//
//	SummarizeForOperator([err_internal, err_internal], "REQ-1")
//	  → "2 internal error(s). Reference: REQ-1"
//
//	SummarizeForOperator([err_safe], "")
//	  → "stripe tax: customer has no country on billing profile"
func SummarizeForOperator(errs []error, requestID string) string {
	if len(errs) == 0 {
		return ""
	}
	var safe []string
	internal := 0
	for _, e := range errs {
		if e == nil {
			continue
		}
		var sme SafeMessageError
		if errors.As(e, &sme) {
			safe = append(safe, Scrub(sme.OperatorSafeMessage()))
			continue
		}
		msg := e.Error()
		if hasInternalMarkers(msg) {
			internal++
			continue
		}
		// A plain message with no internal markers is operator-safe by
		// default. Pass it through after the PII scrub.
		safe = append(safe, Scrub(msg))
	}
	parts := safe
	if internal > 0 {
		ref := ""
		if requestID != "" {
			ref = fmt.Sprintf(". Reference: %s", requestID)
		}
		parts = append(parts, fmt.Sprintf("%d internal error(s)%s", internal, ref))
	}
	return strings.Join(parts, "; ")
}

// hasInternalMarkers reports whether msg contains a pattern that means
// something only to engineers and support, never to operators: SQLSTATE
// codes, Go panic markers, source file:line references and stack-trace frame
// markers. It is shared so that SanitizeForOperator and SummarizeForOperator
// stay in sync.
func hasInternalMarkers(msg string) bool {
	return reSQLState.MatchString(msg) ||
		reGoFileLine.MatchString(msg) ||
		strings.Contains(msg, "runtime error:") ||
		strings.Contains(msg, "panic:") ||
		strings.Contains(msg, "goroutine ")
}

// reSQLState matches a Postgres or other database SQLSTATE code inside an
// error string, such as "(SQLSTATE 42702)" or "SQLSTATE: 23505".
var reSQLState = regexp.MustCompile(`\bSQLSTATE\s*[:= ]?\s*[0-9A-Z]{5}\b`)

// reGoFileLine matches a Go file:line frame (such as
// "/Users/.../engine.go:1234"), which means a stack frame leaked into an
// error string. It is anchored on the .go: suffix, so it does not match a
// UNIX path in a user-facing message.
var reGoFileLine = regexp.MustCompile(`\.go:\d+\b`)
