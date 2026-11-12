// Package testclock owns the TestClock resource: a tenant-scoped frozen-time
// simulator used to walk test-mode subscriptions through full billing
// lifecycles (trials, cycles, dunning retries) in compressed wall-clock time.
//
// Clocks exist only in test mode — the underlying table and its CHECK
// constraint enforce livemode=false. Live-mode callers cannot create, advance,
// or attach clocks; attempting to reach the endpoints from a live key produces
// a 400 from the service guard before the DB even sees the write.
package testclock

import (
	"context"
	"errors"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// Store is the persistence contract for TestClock rows. Kept narrow: create,
// read, list, atomic status transitions, atomic advance, delete. Every method
// is RLS-scoped to one tenant (postgres.TxTenant) except
// ListAdvancingForCatchup, the catch-up role's list across tenants.
type Store interface {
	Create(ctx context.Context, tenantID string, clk domain.TestClock) (domain.TestClock, error)
	Get(ctx context.Context, tenantID, id string) (domain.TestClock, error)
	// List returns one page of the clocks, newest first, and how many there are.
	List(ctx context.Context, tenantID string, limit, offset int) ([]domain.TestClock, int, error)
	Delete(ctx context.Context, tenantID, id string) error

	// MarkAdvancing flips status ready → advancing and simultaneously sets
	// the new frozen_time, atomically. It restarts last_advance_summary with
	// advanced_from, the frozen_time before this advance. The new
	// frozen_time becomes visible immediately — this is what lets the
	// catch-up find subs on the clock as "due". Returns errs.InvalidState when the clock is
	// not currently ready (prevents overlapping advances).
	//
	// The new frozen_time must be >= the current frozen_time; the service
	// enforces that up-front so we don't ship a regressed-time clock.
	MarkAdvancing(ctx context.Context, tenantID, id string, newFrozenTime time.Time) (domain.TestClock, error)

	// TouchAdvancing starts a catch-up pass: it moves the clock to the back
	// of the claim list.
	TouchAdvancing(ctx context.Context, tenantID, id string) error
	// CompleteAdvance flips status advancing → ready at the end of a
	// successful catch-up, storing its summary in the same statement. Clears
	// any last_failure_reason from a prior failed-then-retried advance.
	CompleteAdvance(ctx context.Context, tenantID, id string, summary domain.AdvanceSummary) (domain.TestClock, error)
	// MarkFailed flips advancing → internal_failure when a catch-up fails,
	// storing its summary so far in the same statement. The reason is
	// persisted on the clock row so the dashboard can show "Catchup failed:
	// <reason>". The store cuts it to about 500 bytes; the full payload
	// stays in structured slog. ADR-018.
	MarkFailed(ctx context.Context, tenantID, id, reason string, summary domain.AdvanceSummary) (domain.TestClock, error)
	// RetryFromFailed transitions internal_failure → advancing on a
	// clock the operator has chosen to retry. Frozen_time is unchanged
	// — the catchup loop only processes subs whose next_billing_at <=
	// frozen_time, so resuming from where the previous attempt
	// stopped is idempotent. The testclock_catchup leader picks the
	// clock up on its next tick, like any other advance. ADR-018.
	RetryFromFailed(ctx context.Context, tenantID, id string) (domain.TestClock, error)

	// ListAdvancingForCatchup is the testclock_catchup role's claim list:
	// the clocks in status='advancing' across ALL tenants, the one touched
	// longest ago first, at most limit. The statement proves the lease's
	// fence (ADR-114), so a deposed leader lists nothing.
	//
	// TouchAdvancing, CompleteAdvance and MarkFailed are the catch-up's
	// writes: each is fenced on the testclock_catchup lease, and a miss
	// returns ErrCatchupFenceLost or ErrClockNoLongerAdvancing.
	ListAdvancingForCatchup(ctx context.Context, limit int) ([]domain.TestClock, error)
}

// The outcomes of a fenced catch-up write (TouchAdvancing, CompleteAdvance,
// MarkFailed) that changed no row. Neither is a failure of the
// advance: the run stops, logs, and leaves the clock as it is.
var (
	// ErrCatchupFenceLost: the clock is still advancing, but this run no
	// longer holds the testclock_catchup lease. The current leader runs the
	// clock's pass again.
	ErrCatchupFenceLost = errors.New("testclock: the catch-up lease was lost; the current leader runs this clock")
	// ErrClockNoLongerAdvancing: the clock is gone, or out of advancing —
	// another run completed or failed it, or the tenant deleted it.
	ErrClockNoLongerAdvancing = errors.New("testclock: the clock is no longer advancing")
)
