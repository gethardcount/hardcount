package audit

import (
	"context"

	"github.com/gethardcount/hardcount/internal/platform/request"
)

// The audit mark answers ONE request-scoped question: "is this request's
// audit story accounted for?" It lives on the request's record
// (request.MarkAudited), and exactly two things set it:
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
// A job or the CLI writes audit rows with no request: the marks are no-ops
// there, which is correct — nothing is observing.

// markEmitted records that an audit row for this request actually landed.
// Unexported ON PURPOSE: only Logger.Log / Logger.LogInTx call it, and only
// after a successful INSERT. A caller that wants to say "nothing happened here"
// says MarkSkip; nobody gets to claim a row that was never written.
func markEmitted(ctx context.Context) { request.MarkAudited(ctx) }

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
func MarkSkip(ctx context.Context) { request.MarkAudited(ctx) }

// WasHandled reports whether this request's audit story is accounted for: a row
// was emitted (Log / LogInTx self-mark), or the owning code declared there was
// nothing to audit (MarkSkip).
//
// The AuditCoverage detector reads it as "this mutation left evidence" — and a
// mutating 2xx for which it is FALSE is an uncovered mutation.
func WasHandled(ctx context.Context) bool { return request.Of(ctx).Audited() }
