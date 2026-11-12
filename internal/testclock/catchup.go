package testclock

// The test-clock catch-up runs everything that fell due in the time an
// advancing clock skipped. It is a leader-leased role, testclock_catchup
// (ADR-140, ADR-114): one replica at a time runs it, on a tick, and every
// write it makes to a clock row proves the role's fence. Advance and Retry
// advance only mark the clock; the leader runs it on a later tick. A
// process that stops mid-pass leaves the clock advancing, and the next
// leader runs the pass again: every phase is safe to repeat (its own
// compare-and-set guards), which is what a repeated pass relies on.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/platform/scheduler"
)

const (
	// CatchupTickInterval is the testclock_catchup role's cadence. The
	// runner polls at min(interval, leader.MaxPoll). An advance starts
	// within about one interval when no other catch-up is running;
	// otherwise it waits for the passes ahead of it, each up to
	// CatchupPassDeadline. A tick with no advancing clock is one fenced
	// SELECT.
	CatchupTickInterval = 2 * time.Second

	// CatchupPassDeadline bounds one catch-up pass on one clock. A pass that
	// has not finished by then ends the advance in internal_failure, with a
	// reason that names the clock's time, and Retry advance runs it again.
	// One pass covers the whole advance, so it is generous.
	CatchupPassDeadline = 10 * time.Minute

	// catchupClocksPerTick caps how many clocks one tick works. The rest
	// wait for the next tick, which starts with the one whose pass started
	// longest ago.
	catchupClocksPerTick = 20
)

// RunCatchupLoop runs the testclock_catchup role until ctx ends: each tick
// this replica leads runs CatchupTick.
func (s *Service) RunCatchupLoop(ctx context.Context, gate leader.Gate) {
	scheduler.Run(ctx, leader.RoleTestClockCatchup, CatchupTickInterval, gate, s.CatchupTick, nil)
}

// CatchupTick is the role's tick body (scheduler.WorkFunc). It logs its own
// errors.
func (s *Service) CatchupTick(ctx context.Context) {
	worked, err := s.RunAdvancing(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "test clock: catch-up tick could not list the advancing clocks", "error", err)
		return
	}
	if worked > 0 {
		slog.InfoContext(ctx, "test clock: catch-up tick", "clocks", worked)
	}
}

// RunAdvancing lists the advancing clocks with the role's fenced claim list
// and runs one catch-up pass on each, the one touched longest ago first. It
// returns how many clocks it ran. When ctx ends (a shutdown, or the lease
// lost) it stops before the next clock, and those clocks stay advancing for
// the next leader. A clock's failure is the clock's: it is recorded on the
// clock (internal_failure) and logged, and the tick goes on.
func (s *Service) RunAdvancing(ctx context.Context) (int, error) {
	clocks, err := s.store.ListAdvancingForCatchup(ctx, catchupClocksPerTick)
	if err != nil {
		return 0, fmt.Errorf("list advancing clocks: %w", err)
	}
	worked := 0
	for _, clk := range clocks {
		if ctx.Err() != nil {
			break
		}
		// Test clocks exist only in test mode (the table's CHECK).
		if err := s.runClock(postgres.WithLivemode(ctx, false), clk); err != nil {
			slog.ErrorContext(ctx, "test clock: catch-up pass failed",
				"clock_id", clk.ID, "tenant_id", clk.TenantID, "error", err)
		}
		worked++
	}
	return worked, nil
}
