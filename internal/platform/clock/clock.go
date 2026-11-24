// Package clock gives code the current time, by one rule: a request's time
// is bound by the API's middleware (api.bindRequestTime), and work that
// outlives a request, and every job, binds one per row (EnsureEffectiveNow).
// One piece of work reads one time: the instant bound on its ctx, which
// Clock.Now and Now return. With none bound, they read the machine's time.
//
// The one exception is AtArrival: a charge's result, and the invoice's writes
// that follow it, are dated when Stripe's answer arrives.
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
// scheduler, webhook delivery timestamps and the audit log's rows.
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

// instantKey is the ctx key for the bound instant; a zero time means none.
// Its type is unexported, so other packages bind it only through this
// package.
type instantKeyType struct{}

var instantKey = instantKeyType{}

// WithEffectiveNow binds a real instant. Every Clock.Now(ctx) below reads
// this value instead of wall-clock time.
func WithEffectiveNow(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, instantKey, t.UTC())
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
	return WithEffectiveNow(ctx, c.Now(WithoutInstant(ctx)))
}

// EffectiveNow returns the bound instant and true when ctx carries one, from
// WithEffectiveNow. Otherwise it returns the zero time and false.
func EffectiveNow(ctx context.Context) (time.Time, bool) {
	if ctx == nil {
		return time.Time{}, false
	}
	t, ok := ctx.Value(instantKey).(time.Time)
	if !ok || t.IsZero() {
		return time.Time{}, false
	}
	return t, true
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

// WithoutInstant drops any instant ctx carries: Now(ctx) then reads machine
// time. A job's tick starts this way, and so does work a request starts and
// leaves running, so each row it works binds its own instant.
func WithoutInstant(ctx context.Context) context.Context {
	return context.WithValue(ctx, instantKey, time.Time{})
}
