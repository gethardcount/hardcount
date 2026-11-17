package errs

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// SafeMessageError is an error that carries its own text for an operator,
// such as a Stripe card decline with its decline code (ADR-026). Wherever an
// error reaches an operator, OperatorSafeMessage is shown in place of
// Error(). Return from it only text already fit for an operator: never a raw
// provider string, SQL, a stack or a panic.
type SafeMessageError interface {
	error
	OperatorSafeMessage() string
}

// genericInternalError is what an operator sees in place of an error with an
// internal marker. The request ID lets support find the error in the log.
// It is capitalized because the dashboard shows it as a sentence.
func genericInternalError(requestID string) string {
	if requestID == "" {
		return "An internal error occurred. See server logs for details."
	}
	return fmt.Sprintf("An internal error occurred. Reference: %s", requestID)
}

// SanitizeForOperator returns the text of err that an operator may see, in
// the dashboard or in a column it reads (ADR-026), with personal data
// scrubbed (Scrub):
//
//  1. The OperatorSafeMessage of a SafeMessageError in err's chain.
//  2. Else, when the message holds an internal marker (hasInternalMarkers),
//     "An internal error occurred." with requestID as its reference, or a
//     pointer to the server log when requestID is empty. The caller logs
//     err.
//  3. Else the message itself.
//
// It returns "" for a nil err.
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

// SummarizeForOperator joins the operator text of a batch of errors, such as
// a scheduler tick's per-row errors, with "; ". Each error gives what
// SanitizeForOperator would, except that the errors with an internal marker
// are counted into one last part, "N internal error(s)", with requestID as
// its reference when it is set. Nil errors are skipped; it returns "" for an
// empty batch. It never logs: the caller logs the errors. For example, one
// safe error and two with a marker give:
//
//	stripe tax: customer has no country on billing profile; 2 internal error(s). Reference: REQ-1
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

// hasInternalMarkers reports whether msg holds text meant for engineers, not
// operators: a SQLSTATE code, a Go file:line, "runtime error:", "panic:" or
// "goroutine ". There is no list of safe text: a message without a marker is
// shown.
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
