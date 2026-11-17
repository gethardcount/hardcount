package audit

import (
	"context"

	"github.com/gethardcount/hardcount/internal/platform/request"
)

// markEmitted sets the request's audit mark (request.MarkAudited) once an
// audit row for it has landed. The mark answers one question for the
// AuditCoverage detector: is this request's audit story accounted for? It
// has two setters, both in this package: markEmitted, which only Logger.Log
// and Logger.LogInTx call, after a successful INSERT, and MarkSkip.
// request.MarkAudited is exported only so this package can reach the record;
// internal/arch keeps every other package from calling it, so no code claims
// coverage without a row or a declared MarkSkip. A job or the CLI writes rows
// with no request: the mark is a no-op there, and nothing is observing.
func markEmitted(ctx context.Context) { request.MarkAudited(ctx) }

// MarkSkip declares that this request performs no auditable mutation, so the
// AuditCoverage detector must not report it as an uncovered mutation: a
// read-only POST (an invoice or recipe preview), or a no-op branch of a
// mutating route (a logout with a stale cookie, a password reset for an
// unknown email, a settings save that changed no field). Without it those
// paths look, at the transport, like a mutation that forgot its audit row. It
// is a claim about REALITY — "this 2xx changed nothing" — and it is wrong to
// call it on any path that mutated state.
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
// mutating 2xx for which it is FALSE is an uncovered mutation. It panics with
// no record (request.Of): the detector calls it only inside a request.
func WasHandled(ctx context.Context) bool { return request.Of(ctx).Audited() }
