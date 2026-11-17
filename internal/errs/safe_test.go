package errs

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// safeErr is a SafeMessageError whose Error() differs from its safe text.
type safeErr struct{ msg string }

func (e *safeErr) Error() string               { return "wrapped: " + e.msg }
func (e *safeErr) OperatorSafeMessage() string { return e.msg }

// A SafeMessageError gives its OperatorSafeMessage, not its Error().
func TestSanitizeForOperator_SafeMessageErrorGivesItsSafeMessage(t *testing.T) {
	err := &safeErr{msg: "stripe tax: customer has no country on billing profile"}
	got := SanitizeForOperator(err, "REQ-1")
	want := "stripe tax: customer has no country on billing profile"
	if got != want {
		t.Errorf("SafeMessageError must be passed through. got %q, want %q", got, want)
	}
}

// A message with any internal marker gives the generic message in its place,
// with the request ID as its reference, or a pointer to the server log
// without one.
func TestSanitizeForOperator_InternalMarkerGivesTheGenericMessage(t *testing.T) {
	cases := map[string]string{
		"SQLSTATE":      `list pending charges: ERROR: column reference "id" is ambiguous (SQLSTATE 42702)`,
		"Go file:line":  "oh no: /Users/dev/foo/bar.go:123 +0x4b",
		"panic":         "panic: something broke",
		"runtime error": "runtime error: invalid memory address or nil pointer dereference",
		"goroutine":     "goroutine 17 [running]:",
	}
	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			if got, want := SanitizeForOperator(errors.New(msg), "REQ-99"), "An internal error occurred. Reference: REQ-99"; got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
	if got, want := SanitizeForOperator(errors.New(cases["panic"]), ""), "An internal error occurred. See server logs for details."; got != want {
		t.Errorf("no request ID: got %q, want %q", got, want)
	}
}

// A message with no internal marker is shown, with personal data scrubbed.
func TestSanitizeForOperator_PlainMessageIsShownScrubbed(t *testing.T) {
	err := errors.New("payment failed for ops@example.com on card ending 4242")
	got := SanitizeForOperator(err, "REQ-4")
	if strings.Contains(got, "ops@example.com") {
		t.Errorf("email leaked despite Scrub: %q", got)
	}
	if !strings.Contains(got, "payment failed") {
		t.Errorf("operator-safe substance dropped: %q", got)
	}
}

func TestSanitizeForOperator_NilIsEmpty(t *testing.T) {
	if got := SanitizeForOperator(nil, "REQ"); got != "" {
		t.Errorf("nil err must yield empty string; got %q", got)
	}
}

// Safe text is joined, and the errors with a marker are counted with the
// request ID.
func TestSummarizeForOperator_JoinsSafeTextAndCountsMarkedErrors(t *testing.T) {
	errs := []error{
		&safeErr{msg: "stripe tax: customer has no country"},
		errors.New(`ERROR: column reference "id" is ambiguous (SQLSTATE 42702)`),
		errors.New("foo.go:99 nil deref"),
	}
	got := SummarizeForOperator(errs, "REQ-XYZ")
	if !strings.Contains(got, "no country") {
		t.Errorf("safe message dropped: %q", got)
	}
	if strings.Contains(got, "SQLSTATE") {
		t.Errorf("SQLSTATE leaked: %q", got)
	}
	if !strings.Contains(got, "2 internal error(s)") {
		t.Errorf("internal-count rollup missing: %q", got)
	}
	if !strings.Contains(got, "REQ-XYZ") {
		t.Errorf("request_id reference missing: %q", got)
	}
}

func TestSummarizeForOperator_EmptyBatchIsEmpty(t *testing.T) {
	if got := SummarizeForOperator(nil, "REQ"); got != "" {
		t.Errorf("empty errs slice should yield empty string; got %q", got)
	}
	if got := SummarizeForOperator([]error{}, "REQ"); got != "" {
		t.Errorf("empty errs slice should yield empty string; got %q", got)
	}
}

// With no marked error there is no count.
func TestSummarizeForOperator_NoMarkedErrorNoCount(t *testing.T) {
	errs := []error{
		&safeErr{msg: "stripe tax: provider outage"},
		&safeErr{msg: "card declined: insufficient_funds"},
	}
	got := SummarizeForOperator(errs, "REQ-1")
	if strings.Contains(got, "internal error") {
		t.Errorf("no internal errors in input — must not append rollup. got %q", got)
	}
	if !strings.Contains(got, "stripe tax") || !strings.Contains(got, "insufficient_funds") {
		t.Errorf("safe messages must be preserved: %q", got)
	}
}

// With only marked errors the summary is the count alone.
func TestSummarizeForOperator_OnlyMarkedErrorsGiveTheCountAlone(t *testing.T) {
	errs := []error{
		fmt.Errorf("ERROR: deadlock detected (SQLSTATE 40P01)"),
		fmt.Errorf("ERROR: relation does not exist (SQLSTATE 42P01)"),
	}
	got := SummarizeForOperator(errs, "REQ-2")
	if strings.Contains(got, "SQLSTATE") {
		t.Errorf("internal SQL leaked: %q", got)
	}
	if !strings.HasPrefix(got, "2 internal error(s)") {
		t.Errorf("expected '2 internal error(s)' prefix; got %q", got)
	}
}
