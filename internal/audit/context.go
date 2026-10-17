package audit

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
)

type ctxKey int

const (
	stateKey ctxKey = iota + 1
	ipKey
)

// requestState answers ONE request-scoped question: "is this request's audit
// story accounted for?" Exactly two things set it:
//
//   - a successful emission — Logger.Log and Logger.LogInTx self-mark, so any
//     real audit row for this request counts, wherever in the call stack it was
//     written (handler, service, or an in-tx emission deep inside a store).
//   - MarkSkip: the code that owns the decision declaring this request mutated
//     nothing, so there is nothing to audit — a read-only POST (invoice/recipe
//     *preview*), or a no-op branch of a mutating route (a logout with a stale
//     cookie, a password-reset for an unknown email, a settings save that
//     changed no field, an idempotency replay, a recipe re-apply that installs
//     nothing). Without this declaration those paths are indistinguishable, to
//     an observer at the transport, from a mutation that FORGOT its audit row —
//     and a detector that cries wolf on a normal client retry is a detector
//     nobody keeps.
//
// One reader consumes it: the AuditCoverage detector (a pure observer — a
// mutating 2xx that is not accounted for is an UNCOVERED MUTATION, the thing
// the deleted catch-all used to paper over with a guessed row).
//
// There is deliberately no exported "I wrote a row" mark. The old MarkHandled
// let a handler ASSERT coverage; the only thing that may assert coverage now is
// an audit row that actually landed.
//
// Kept as a pointer so a mark made anywhere downstream is visible back in the
// middleware frame through the ctx value lookup.
type requestState struct {
	accounted atomic.Bool
}

// WithRequestState seeds the bookkeeping cell on the request context. The
// AuditCoverage detector does this once, at the root, for every request.
//
// IDEMPOTENT BY CONTRACT: if a cell is already present, the SAME cell is kept.
// A second seed would SHADOW the first — emissions would mark the inner cell
// while the detector read the outer, so every covered route would look
// uncovered. One request, one cell.
//
// A ctx that was never seeded (background jobs, CLI callers) makes the marks
// no-ops, which is correct — nothing is observing.
func WithRequestState(ctx context.Context) context.Context {
	if _, ok := ctx.Value(stateKey).(*requestState); ok {
		return ctx
	}
	return context.WithValue(ctx, stateKey, &requestState{})
}

// markEmitted records that an audit row for this request actually landed.
// Unexported ON PURPOSE: only Logger.Log / Logger.LogInTx call it, and only
// after a successful INSERT. A caller that wants to say "nothing happened here"
// says MarkSkip; nobody gets to claim a row that was never written.
func markEmitted(ctx context.Context) {
	if s, ok := ctx.Value(stateKey).(*requestState); ok {
		s.accounted.Store(true)
	}
}

// MarkSkip declares that this request performs no auditable mutation, so the
// AuditCoverage detector must not report it as an uncovered mutation. It is a
// claim about REALITY — "this 2xx changed nothing" — and it is wrong to call it
// on any path that mutated state.
//
// The live callers are NOT enumerated here. That list drifted in three
// consecutive completeness audits — a caller was added, the prose was not, and a
// paragraph that reads as exhaustive quietly stopped being so.
//
// It is now DERIVED FROM THE CODE and pinned by
// internal/arch/audit_prose_gates_test.go (markSkipCallers): add a MarkSkip call
// without declaring it there and CI fails, naming the file. Read that table for
// the current set and the reason each one is legitimate. Prose that makes a
// precise, checkable claim and is never checked will drift; this one is checked.
func MarkSkip(ctx context.Context) {
	if s, ok := ctx.Value(stateKey).(*requestState); ok {
		s.accounted.Store(true)
	}
}

// WasHandled reports whether this request's audit story is accounted for: a row
// was emitted (Log / LogInTx self-mark), or the owning code declared there was
// nothing to audit (MarkSkip).
//
// The AuditCoverage detector reads it as "this mutation left evidence" — and a
// mutating 2xx for which it is FALSE is an uncovered mutation.
func WasHandled(ctx context.Context) bool {
	if s, ok := ctx.Value(stateKey).(*requestState); ok {
		return s.accounted.Load()
	}
	return false
}

// WithClientIP stashes the request's source IP so downstream audit writes
// (middleware or explicit Logger.Log) record the same value.
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, ipKey, ip)
}

// ClientIP returns the IP stashed by WithClientIP, or "" if none.
func ClientIP(ctx context.Context) string {
	if ip, ok := ctx.Value(ipKey).(string); ok {
		return ip
	}
	return ""
}

// ExtractClientIP returns the source IP recorded on audit rows and on the
// dashboard session rows that login and invite accept write: the host of
// r.RemoteAddr, which the global TrustedRealIP middleware has already resolved
// from X-Forwarded-For / X-Real-IP — but ONLY when the immediate peer is a
// configured trusted proxy. It deliberately does NOT re-read those headers
// itself: doing so trusted them unconditionally, so any client could forge
// audit_log.ip_address (and the boot log even claims headers are ignored when
// TRUST_PROXY is unset). Deferring to TrustedRealIP makes the audit IP match
// the real-IP policy the rest of the stack (rate limiter, logs) already uses.
func ExtractClientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
