package arch

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// WHY THIS GATE EXISTS
//
// audit_log is APPEND-ONLY, and migration 0150 revoked DELETE and TRUNCATE from
// the runtime role. Whatever a writer puts into a row is there forever: it cannot
// be corrected, redacted, or erased by the application, ever. That makes the audit
// log the one table in Hardcount from which a person's email address can never be
// removed — a GDPR-erasure dead end.
//
// The rule is therefore: an audit row POINTS AT a person (resource_id = their user
// id, or the id of an erasable row like a member_invitations record). It never
// STORES their address. The reader resolves the address at query time by joining
// the mutable, erasable table (internal/audit/audit.go, auditListSelect), so
// deleting the person deletes the address from every historical row at once.
//
// There WAS a guard for this. It caught nothing, because it checked only:
//   - metadata (not resource_label, which is where the addresses actually were),
//   - values that assert to string (so a struct or a []string sailed through), and
//   - the two customer writers (so auth, members and bootstrap were never looked at).
//
// Under it, SEVEN writers were putting live email addresses into the append-only
// log: login, password_reset_requested, password_reset_completed, member.invited
// (label AND metadata), member.joined, and bootstrap's owner_email. A guard whose
// scope is narrower than the rule it guards is not a guard.
//
// This one is source-level and repo-wide on purpose: it cannot be satisfied by a
// writer the author forgot to add to a list.

// auditEmitCall matches a call that writes an audit row, in any of its shapes.
var auditEmitCall = regexp.MustCompile(`(?:\.Log|\.LogInTx|auditEvent|auditAuthEvent|auditInvoiceFinalized)\(`)

// emailBearingArg matches an expression that is an email ADDRESS rather than an id:
// `u.Email`, `res.Email`, `inv.Email`, `req.Email`, `ownerEmail`, `"email":`.
// Deliberately syntactic — it reads what the writer HANDS OVER, which is the thing
// that becomes permanent.
var emailBearingArg = regexp.MustCompile(`\b\w*[Ee]mail\b`)

// flagComparisonTail matches what must FOLLOW an email identifier for the
// occurrence to be the CORRECT pattern — a boolean flag (`out.Email != ""`)
// recording WHETHER an address exists, not the address. Matched against the
// text immediately after the identifier, per occurrence.
var flagComparisonTail = regexp.MustCompile(`^\s*[!=]=\s*""`)

// stripComments removes // prose so the gate reads CODE, not the explanation of it.
func stripComments(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// idish are the expressions that are FINE to hand an audit row: ids, not addresses.
var idish = regexp.MustCompile(`\b(\w*ID|\w*Id|\w*_id)\b`)

// TestNoEmailAddressesEnterTheAppendOnlyLog fails when a call that writes an audit
// row is handed an email-bearing expression.
func TestNoEmailAddressesEnterTheAppendOnlyLog(t *testing.T) {
	var offenders []string

	for path, src := range sourceFiles(t) {
		// The audit package itself RESOLVES emails at read time (that is the fix,
		// not the bug), and the security log deliberately records failed-login
		// emails outside audit_log.
		if strings.HasPrefix(path, "internal/audit/") {
			continue
		}
		lines := strings.Split(src, "\n")
		for i, line := range lines {
			if !auditEmitCall.MatchString(line) {
				continue
			}
			// An emit call can span lines. The first version of this window was
			// a fixed 8 lines with a "})" cutoff — an address on line 9+ of a
			// metadata literal, or after a nested close, escaped the gate. Now
			// the window follows the call's own paren balance to its closing
			// paren (capped defensively), so the gate reads the WHOLE argument
			// list however long the literal grows.
			call := collectCallText(lines, i, 60)

			// Strip comments before matching. The first version of this gate fired on
			// its own explanatory prose ("an email written here could never be
			// erased") — a gate that cries wolf on a correct sentence is a gate the
			// next person deletes.
			call = stripComments(call)

			for _, loc := range emailBearingArg.FindAllStringIndex(call, -1) {
				m := call[loc[0]:loc[1]]
				// `email_set`, `email_changed`, `email_status` are FLAGS, not
				// addresses — recording the flag instead of the value is exactly
				// the pattern this rule asks for.
				if strings.Contains(strings.ToLower(m), "email_set") ||
					strings.Contains(strings.ToLower(m), "email_changed") ||
					strings.Contains(strings.ToLower(m), "email_status") ||
					strings.Contains(strings.ToLower(m), "emailset") {
					continue
				}
				if idish.MatchString(m) {
					continue
				}
				// `X.Email != ""` is a boolean FLAG, not the address. The check
				// is PER OCCURRENCE — the first version exempted the whole call
				// if a flag appeared anywhere in it, so an address co-passed
				// BESIDE a legitimate flag sailed through. Only the identifier
				// that is itself compared to "" is exempt.
				if flagComparisonTail.MatchString(call[loc[1]:]) {
					continue
				}
				offenders = append(offenders, path+":"+itoa(i+1)+": "+strings.TrimSpace(line))
				break
			}
		}
	}

	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf(`%d audit write(s) hand an EMAIL ADDRESS to an append-only row:

  %s

audit_log cannot be corrected, redacted or erased — migration 0150 revoked DELETE
from the runtime role. An address written here is permanent, which makes this table
a GDPR-erasure dead end for that person.

Do not store the address. POINT AT the person instead:

    resource_id = <their user id>          (or the id of an erasable row)
    resource_label = ""                    (the reader resolves it)

internal/audit/audit.go's auditListSelect joins users / member_invitations and
resolves the address at READ time, so deleting the person deletes it from every
historical row at once. Record a FLAG (email_set, email_changed) when you only need
to know that an address was involved.`,
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

// collectCallText joins lines from the emit call until its parenthesis balance
// closes (or maxLines as a defensive cap). Balance is counted on a copy with
// string literals blanked, so a ")" inside a description cannot end the window
// early; email matching runs on the REAL text.
func collectCallText(lines []string, start, maxLines int) string {
	depth := 0
	var parts []string
	for i := start; i < len(lines) && i < start+maxLines; i++ {
		parts = append(parts, lines[i])
		for _, r := range blankStrings(lines[i]) {
			switch r {
			case '(':
				depth++
			case ')':
				depth--
			}
		}
		if depth <= 0 && i > start {
			break
		}
		if depth <= 0 && strings.Contains(blankStrings(lines[i]), ")") {
			break
		}
	}
	return strings.Join(parts, " ")
}

// blankStrings replaces the contents of double-quoted string literals with
// spaces so parens inside them do not affect balance counting.
func blankStrings(line string) string {
	out := []rune(line)
	inStr := false
	for i := 0; i < len(out); i++ {
		switch {
		case out[i] == '\\' && inStr:
			if i+1 < len(out) {
				out[i], out[i+1] = ' ', ' '
				i++
			}
		case out[i] == '"':
			inStr = !inStr
		case inStr:
			out[i] = ' '
		}
	}
	return string(out)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
