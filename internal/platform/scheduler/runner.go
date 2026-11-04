// Package scheduler hosts the standard tick-loop runner that every
// ticker-driven singleton worker in Hardcount uses. The authoritative list
// is `grep -rn "scheduler.Run("`. There are five: billing (1h in production,
// 5m in local), dunning (the same interval), webhook_outbox (2s),
// email_outbox (5s) and webhook_delivery (30s). The test-clock catch-up
// worker is driven by a queue, not a ticker, so it does not use this runner.
//
// Every loop is leader-gated (ADR-114). A tick runs only when this replica
// leads the role for that tick, on a ctx carrying the lease token that the
// role's claim statements prove. The gate is a required parameter, so a
// loop cannot forget it. The runner polls at min(interval, leader.MaxPoll),
// so a dead leader's role runs elsewhere within LeaseTTL + MaxPoll.
//
// The runner keeps each worker to about three lines, with one place for
// panic recovery, heartbeat logging and the leader gate.
package scheduler

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/leader"
)

// heartbeatThreshold divides slow roles from fast ones. A slow role logs at
// info level when it leads a tick. A fast role logs at debug level, because
// an info line per tick would only be log-volume noise.
const heartbeatThreshold = 1 * time.Minute

// WorkFunc is the per-tick body. It must be safe to call repeatedly, and it
// must return cleanly when ctx is cancelled. The ctx is cancelled with cause
// leader.ErrLeaseLost if the lease cannot be kept. A WorkFunc logs its own
// errors.
type WorkFunc func(ctx context.Context)

// Run drives a leader-gated tick loop for role until ctx is cancelled.
//
// Each poll, every min(interval, leader.MaxPoll), does two things:
//   - onPoll fires if it is not nil. The billing loop uses it to stamp this
//     replica's liveness for /health/ready. Followers stamp too, which is
//     the current contract.
//   - gate.Lead tries to take one tick of the role. If the role is not due,
//     is held elsewhere, or is paused, nothing happens. If this replica
//     leads, workFn runs on the led ctx.
//
// A panic in workFn unwinds through the gate, which releases the lease as
// not completed, so the stall gauge ages. Run then recovers the panic and
// logs it at error level with the stack. A gate error is logged at error
// level once per poll, with no rate limiter.
func Run(ctx context.Context, role leader.Role, interval time.Duration, gate leader.Gate, workFn WorkFunc, onPoll func()) {
	if gate == nil {
		slog.Error("scheduler: no leader gate wired — refusing to run an ungated singleton loop", "role", role)
		return
	}
	poll := interval
	if poll > leader.MaxPoll {
		poll = leader.MaxPoll
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	level := slog.LevelInfo
	if interval < heartbeatThreshold {
		level = slog.LevelDebug
	}

	for {
		select {
		case <-ctx.Done():
			slog.Info("scheduler stopped", "worker", role)
			return
		case <-ticker.C:
			if onPoll != nil {
				onPoll()
			}
			start := time.Now()
			led, err := leadOneTick(ctx, role, interval, gate, workFn)
			if err != nil {
				slog.Error("scheduler: leader gate error", "worker", role, "error", err)
				continue
			}
			if led {
				slog.Log(ctx, level, "scheduler tick led", "worker", role, "took", time.Since(start).Round(time.Millisecond).String())
			}
		}
	}
}

// leadOneTick runs one gate.Lead with the recover() around it, not inside
// the work, so a panic in workFn unwinds through Lead. That is how the gate
// learns the tick did not complete. A recover inside the work would make
// Lead see a normal return and record a completed tick. The stall gauge
// would then stay fresh while a panicking tick did nothing every interval.
//
// It logs the recovered value and a stack at error level. Inside a deferred
// call, the panicking frames are still on the stack. The caller's next poll
// fires normally. A WorkFunc that runs its steps under Panics (billing,
// dunning) raises the first panic again itself, so this stack ends at
// Repanic. The panic's own stack is on the first "step panicked; running the
// rest" line, because each guard the panic passes on the way up logs one.
func leadOneTick(ctx context.Context, role leader.Role, interval time.Duration, gate leader.Gate, workFn WorkFunc) (led bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduler panic recovered",
				"worker", role,
				"panic", r,
				"stack", string(debug.Stack()),
			)
			led, err = false, nil
		}
	}()
	return gate.Lead(ctx, role, interval, workFn)
}
