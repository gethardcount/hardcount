// Package clock gives code the current time. A ctx may carry a bound
// instant, and Now then returns that instant instead of the system time.
//
// One piece of work runs on one time. On every route but a decidedIn one the
// API's door (api.clockDoor) binds the request's instant before the handler
// runs. A function that binds its own time (a decidedIn route's function, a
// public page, a Stripe event) calls EnsureEffectiveNow with the service's
// Clock: an instant ctx already carries is kept, else one read of that Clock
// is bound. Every read of the clock below then answers that instant. Work
// over many records (a job a request starts, or a fan-out after the request
// has answered) binds no instant for all of them: each row binds its own.
//
// One exception: a charge's result, and the invoice's writes that follow it
// in the same action, are dated when Stripe's answer arrives (AtArrival), in
// the same call or by webhook. A Stripe Tax answer is not one: it keeps the
// action's instant.
package clock

import (
	"context"
	"sync"
	"time"
)

// Clock provides the current time. Through ctx, a caller can carry a
// request-scoped effective-now: the instant the request treats as now. Entry
// points bind it once (EnsureEffectiveNow), and every call below them, stores
// included, reads that one instant. Without one, callers get wall-clock time.
//
// Code that wants wall-clock time whatever ctx carries calls
// time.Now().UTC() directly, by design. Examples are the cron tick
// scheduler, webhook delivery timestamps and the audit log's recorded_at.
type Clock interface {
	// Now returns ctx's effective-now if one is bound
	// (clock.WithEffectiveNow), and otherwise wall-clock time in UTC.
	Now(ctx context.Context) time.Time
}

// Real returns a clock that uses the system time when ctx has no bound
// effective-now, and the bound value otherwise.
func Real() Clock { return realClock{} }

type realClock struct{}

func (realClock) Now(ctx context.Context) time.Time {
	if t, ok := EffectiveNow(ctx); ok {
		return t
	}
	return time.Now().UTC()
}

// Fake is a controllable clock for testing. An effective-now bound on ctx
// still wins over Fake's `current` field. A test can then exercise the ctx
// binding and still keep Fake's fixed value for each test.
type Fake struct {
	mu      sync.Mutex
	current time.Time
}

// NewFake creates a fake clock fixed at the given time.
func NewFake(t time.Time) *Fake {
	return &Fake{current: t.UTC()}
}

// Now returns ctx's effective-now if one is bound, and otherwise the fake's
// current time.
func (c *Fake) Now(ctx context.Context) time.Time {
	if t, ok := EffectiveNow(ctx); ok {
		return t
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

// Set changes the clock to a specific time.
func (c *Fake) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = t.UTC()
}

// Advance moves the clock forward by the given duration.
func (c *Fake) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = c.current.Add(d)
}

// Sim is the time a context carries: an instant (At), or none (a zero Sim,
// WithRealTime).
type Sim struct {
	At time.Time
}

// simKey is the ctx key for the bound Sim. Its type is unexported, so other
// packages can set the binding only through WithSim or WithEffectiveNow.
type simKeyType struct{}

var simKey = simKeyType{}

// WithSim binds s to ctx. Every Clock.Now(ctx) below reads s.At when it is
// set.
func WithSim(ctx context.Context, s Sim) context.Context {
	s.At = s.At.UTC()
	return context.WithValue(ctx, simKey, s)
}

// WithEffectiveNow binds a real instant. Every Clock.Now(ctx) below reads
// this value instead of wall-clock time.
func WithEffectiveNow(ctx context.Context, t time.Time) context.Context {
	return WithSim(ctx, Sim{At: t})
}

// EnsureEffectiveNow returns ctx with an effective now bound: the one ctx
// carries, else c's now, read once and bound as a real instant. Work that
// read the clock more than once would stamp several instants on one piece of
// work; binding here gives everything below one instant.
func EnsureEffectiveNow(ctx context.Context, c Clock) context.Context {
	if _, ok := EffectiveNow(ctx); ok {
		return ctx
	}
	return WithEffectiveNow(ctx, c.Now(ctx))
}

// AtArrival returns ctx for the package doc's one exception: a charge's
// result and the invoice's writes that follow it. c is read once, and that
// read replaces any instant ctx carries, so the result is dated when it
// arrived.
func AtArrival(ctx context.Context, c Clock) context.Context {
	return WithEffectiveNow(ctx, c.Now(WithRealTime(ctx)))
}

// EffectiveNow returns the bound instant and true when ctx carries one, from
// WithEffectiveNow or WithSim. Otherwise it returns the zero time and false.
func EffectiveNow(ctx context.Context) (time.Time, bool) {
	if ctx == nil {
		return time.Time{}, false
	}
	s, ok := ctx.Value(simKey).(Sim)
	if !ok || s.At.IsZero() {
		return time.Time{}, false
	}
	return s.At, true
}

// Now returns the bound instant if ctx has one, and wall-clock time
// otherwise. It means the same as Real().Now(ctx), with no allocation and no
// Clock field on the caller.
//
// Postgres stores and other infrastructure code use it: they own no Clock
// field but still need timestamps that follow ctx. Service code should keep
// using s.clock.Now(ctx), so tests can pin a Fake clock. Stores read ctx
// directly because in production they behave as Real() does, and their
// tests bind ctx directly.
func Now(ctx context.Context) time.Time {
	if t, ok := EffectiveNow(ctx); ok {
		return t
	}
	return time.Now().UTC()
}

// WithRealTime drops any instant ctx carries: Now(ctx) then reads machine
// time. A job's tick starts this way, so each row it works binds its own
// instant.
func WithRealTime(ctx context.Context) context.Context {
	return WithSim(ctx, Sim{})
}
