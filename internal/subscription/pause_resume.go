package subscription

import (
	"context"
	"database/sql"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// PauseResume is the resume of a lifted pause: one whose resumes_at has
// passed. An operator's timed pause lifts at its date; a dunning pause
// lifts when the invoice it waits for is paid (ReleaseDunningPauseTx
// stamps resumes_at). The row keeps a lifted pause until the scheduled
// resumer clears it, after any period that ended under it has closed.
//
// Whatever removes a lifted pause from the row announces this resume, in
// the same transaction: the scheduled resumers, a new pause (dunning's or
// the operator's), and the operator's resume call. All of them go through
// PostgresStore.clearLiftedPauseTx, so the trigger, the instant, the audit
// row and the event are the same whichever writer gets there first.
type PauseResume struct {
	// At is the pause's resumes_at: when collection resumed.
	At time.Time
	// TriggeredBy is "payment" for a dunning pause and "schedule" for an
	// operator's timed pause.
	TriggeredBy string
}

// liftedPauseResume is the resume of the lifted pause pc.
func liftedPauseResume(pc domain.PauseCollection) PauseResume {
	r := PauseResume{TriggeredBy: "schedule"}
	if pc.ResumesAt != nil {
		r.At = pc.ResumesAt.UTC()
	}
	if pc.SetBy == domain.PauseSetByDunning {
		r.TriggeredBy = "payment"
	}
	return r
}

// eventExtras are subscription.collection_resumed's extras.
func (r PauseResume) eventExtras() map[string]any {
	return map[string]any{"resumed_at": r.At, "triggered_by": r.TriggeredBy}
}

// auditMetadata is the "Collection resumed" audit row's metadata.
func (r PauseResume) auditMetadata(cleared domain.Subscription) map[string]any {
	return map[string]any{
		"action":       domain.SubAuditCollectionResumed,
		"customer_id":  cleared.CustomerID,
		"resumed_at":   r.At,
		"triggered_by": r.TriggeredBy,
	}
}

// ResumeAudit writes the "Collection resumed" audit row for a lifted
// pause the store clears, in the clearing transaction. Its error rolls the
// clear back. Nil writes no row.
//
// The row's actor is the request that runs the clear, as for every audit
// row (audit.ResolveActor): system on the wall-clock resumer, and the
// operator's key or user when their pause or resume call clears it, or
// when their test-clock advance runs the resumer. TriggeredBy says what
// ended the pause.
type ResumeAudit func(ctx context.Context, tx *sql.Tx, cleared domain.Subscription, r PauseResume) error
