package scheduler

import (
	"log/slog"
	"runtime/debug"
)

// Panics keeps one tick's work going past a panic in one of its parts. A part
// is a step of the tick, or one row of a sweep. Run each part with
// `defer p.Guard(...)` at its top, and call Repanic once all the parts have
// run:
//
//	var p scheduler.Panics
//	for _, row := range rows {
//		func() {
//			defer p.Guard("row_id", row.ID)
//			handle(row)
//		}()
//	}
//	p.Repanic()
//
// A part that panics stops, and the others still run. Repanic then raises
// the first panic again, so the tick still ends as panicked: the leader gate
// releases it as not completed, and the lease-lost counter pages. Without
// this, a panic on one row stopped every later step for every tenant, and the
// next tick panicked on the same row again.
//
// The zero value is ready to use. A Panics is not safe for concurrent use.
type Panics struct {
	first any
}

// Guard recovers a panic in the part it is deferred in. It logs the panic at
// ERROR with the stack and args, which name the part (a step, or a row id),
// and keeps the first panic for Repanic. Guard must be the deferred call
// itself (`defer p.Guard(...)`); called any other way it recovers nothing.
func (p *Panics) Guard(args ...any) {
	r := recover()
	if r == nil {
		return
	}
	// The stack is taken here, while the panicking frames are still on it.
	// The scheduler's own log line, after Repanic, shows only Repanic.
	slog.Error("step panicked; running the rest", append(args, "panic", r, "stack", string(debug.Stack()))...)
	if p.first == nil {
		p.first = r
	}
}

// Panicked reports whether Guard kept a panic. The panic Repanic raises drops
// the caller's return values, so a caller that collected errors for its own
// caller to log logs them itself first.
func (p *Panics) Panicked() bool { return p.first != nil }

// Repanic raises the first panic Guard kept, if any.
func (p *Panics) Repanic() {
	if p.first != nil {
		panic(p.first)
	}
}
