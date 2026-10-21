package clock

import (
	"context"
	"sync"
	"time"
)

// Clock provides the current time. The ctx parameter lets callers ride
// a request-scoped effective-now (a test clock's frozen_time) plumbed
// via WithEffectiveNow at operator entry points; without one, callers
// get wall-clock.
//
// Why ctx-aware, not bare Now(): on a clock-pinned billing entity,
// every business-logic timestamp must be in the test clock's
// frozen_time domain (Stripe's "no semantic change" guarantee). A
// bare wall-clock Now() forces every callsite to remember to resolve
// the pin manually — exactly the leak we hit shipping ADR-029. With
// a ctx-aware Now(ctx), services bind effective-now once at the
// operator entry point and every downstream callsite (including
// stores) inherits automatically.
//
// Callers that legitimately want wall-clock regardless of pinning
// (cron tick scheduler, webhook delivery timestamps, postgres
// updated_at columns, audit-log recorded_at) call time.Now().UTC()
// directly — we don't go through Clock for those, by design.
type Clock interface {
	// Now returns ctx's effective-now if bound (clock.WithEffectiveNow),
	// otherwise wall-clock UTC. The ctx-binding shape replaces the
	// per-callsite ClockResolver pattern shipped in earlier ADR-029
	// fixes — ctx propagation gives compositional correctness without
	// every callsite having to remember to resolve.
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

// Fake is a controllable clock for testing. The bound effective-now
// from ctx still wins over Fake's `current` field — tests that want to
// exercise the ctx-binding path can do so without losing Fake's
// per-test deterministic value.
type Fake struct {
	mu      sync.Mutex
	current time.Time
}

// NewFake creates a fake clock fixed at the given time.
func NewFake(t time.Time) *Fake {
	return &Fake{current: t.UTC()}
}

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

// Sim is the SIMULATED-TIME CONTEXT of a code path: which test clock's
// world it is acting in (TestClockID) and the simulated instant it lands
// at (At). The halves travel together and are only ever set from one read
// of the clock — "clock C, at wall-clock now" is a lie, and the audit log
// it would be stamped into is append-only, so the pairing is enforced by
// the type rather than by remembering.
//
// TestClockID == "" means "not in any simulation": At is wall-clock, or a
// bare WithEffectiveNow binding whose clock is not known, and nothing is
// stamped on the sim axis.
type Sim struct {
	At          time.Time
	TestClockID string
}

// Simulated reports whether this context is inside a test clock's world.
// BOTH halves are required: a clock id without an instant (or an instant
// without a clock) is a partial binding, and every consumer treats it as
// absent — a half-stamped audit row would land in the partial clock index
// and then answer sim-time queries with a wall-clock timestamp.
func (s Sim) Simulated() bool {
	return s.TestClockID != "" && !s.At.IsZero()
}

// simKey is the ctx key for the bound Sim. Private type so external
// packages can't poke the binding without going through WithSim /
// WithEffectiveNow.
type simKeyType struct{}

var simKey = simKeyType{}

// WithSim binds a full simulated-time context (instant + clock) to ctx.
// Called where the clock is KNOWN: the ForClock/catchup drivers (which
// hold the clock id and its frozen_time) and BindEffectiveNow (which
// resolves an entity's pin). Every downstream Clock.Now(ctx) reads s.At,
// and every audit emission on this ctx is stamped onto the sim axis
// (audit_log.sim_effective_at / test_clock_id) by the Logger itself —
// ADR-090 §5.
//
// The contract is the one ADR-029 already established for time: "binding
// at the boundary, inheritance below it." A new emitter does not have to
// remember to stamp the sim axis — which is exactly why stamping was
// partial while every emitter had to opt in by hand.
func WithSim(ctx context.Context, s Sim) context.Context {
	s.At = s.At.UTC()
	return context.WithValue(ctx, simKey, s)
}

// SimOf returns the bound simulated-time context. ok is true ONLY for a
// complete binding on a real clock (see Sim.Simulated): a bare
// WithEffectiveNow binding reports false, because its clock is unknown and
// inventing one would fabricate evidence in an append-only log.
func SimOf(ctx context.Context) (Sim, bool) {
	if ctx == nil {
		return Sim{}, false
	}
	s, ok := ctx.Value(simKey).(Sim)
	if !ok || !s.Simulated() {
		return Sim{}, false
	}
	return s, true
}

// WithEffectiveNow binds an effective-now instant whose clock is unknown.
// Every downstream Clock.Now(ctx) reads this value instead of wall-clock.
//
// It deliberately CLEARS any clock id already bound on ctx: rebinding a
// bare instant means "this path's time no longer comes from a clock I can
// name," and carrying the previous clock id forward would stamp rows with
// a clock the new instant does not belong to. Callers that know the clock
// call WithSim.
func WithEffectiveNow(ctx context.Context, t time.Time) context.Context {
	return WithSim(ctx, Sim{At: t})
}

// EffectiveNow returns the bound instant and true when ctx carries any
// binding (WithEffectiveNow or WithSim); zero time and false otherwise.
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

// Now is the package-level shortcut for "the simulated time if ctx is
// bound, wall-clock otherwise" — same semantics as Real().Now(ctx) but
// with zero allocation overhead and no clock-field dep on the caller.
//
// Used by postgres stores and other infrastructure code that doesn't
// own a Clock struct field but still needs ctx-aware timestamps.
// Service-layer code should keep using s.clock.Now(ctx) so tests can
// pin a Fake clock; stores read directly from ctx because their
// production behavior is identical to Real() and tests bind ctx
// directly.
func Now(ctx context.Context) time.Time {
	if t, ok := EffectiveNow(ctx); ok {
		return t
	}
	return time.Now().UTC()
}

// Resolver maps an entity reference to its simulated-time context — the
// clock it is pinned to and that clock's frozen_time, or a bare wall-clock
// Sim (TestClockID "") when the entity is not pinned. Used at operator
// entry points to bind ctx via BindEffectiveNow, and at boundary code
// paths (Stripe webhook handlers, async workers) where there's no
// inherited ctx binding.
//
// It returns the instant and the clock id TOGETHER, from one resolution,
// because they are two halves of one fact: the audit sim axis stamped from
// a clock id resolved separately from its instant is how you get a row
// claiming "clock C at wall-clock now" (ADR-090 §5).
//
// Implemented by *billing.Engine. The platform/clock package owns the
// interface to avoid an import cycle (every domain that needs binding
// can depend on clock without depending on billing).
type Resolver interface {
	SimForCustomer(ctx context.Context, tenantID, customerID string) (Sim, error)
	SimForSubscription(ctx context.Context, tenantID, subscriptionID string) (Sim, error)
	SimForInvoice(ctx context.Context, tenantID, invoiceID string) (Sim, error)
}

// Pin describes which entity to resolve effective-now from. Exactly
// one of CustomerID / SubscriptionID / InvoiceID must be non-empty;
// callers pick the most specific id available. TenantID is always
// required.
type Pin struct {
	TenantID       string
	CustomerID     string
	SubscriptionID string
	InvoiceID      string
}

// BindEffectiveNow resolves a Pin via the resolver and returns a ctx
// carrying that entity's simulated-time context: its effective-now AND,
// when the entity is clock-pinned, the clock id. On resolver error,
// returns the original ctx unmodified — failing the operator's action on
// a dangling pin is worse than stamping the wrong domain on an edge-case
// row. The caller can log the error if they care; most don't.
//
// Binding the clock id here (not just the instant) is what makes audit
// sim-axis stamping TOTAL: every emitter downstream of a pin resolution
// inherits the clock, so no emitter has to know test clocks exist. The
// name is unchanged because the call sites' intent is unchanged — "bind
// this entity's time domain onto ctx"; the clock id is part of that
// domain, and always was.
//
// The returned bool is true when the ctx now carries effective-now,
// false when the binding was skipped (no resolver, no ids, or error).
func BindEffectiveNow(ctx context.Context, r Resolver, p Pin) (context.Context, bool) {
	if r == nil {
		return ctx, false
	}
	var (
		sim Sim
		err error
	)
	switch {
	case p.InvoiceID != "":
		sim, err = r.SimForInvoice(ctx, p.TenantID, p.InvoiceID)
	case p.SubscriptionID != "":
		sim, err = r.SimForSubscription(ctx, p.TenantID, p.SubscriptionID)
	case p.CustomerID != "":
		sim, err = r.SimForCustomer(ctx, p.TenantID, p.CustomerID)
	default:
		return ctx, false
	}
	if err != nil {
		return ctx, false
	}
	// A nested resolution may REFINE the time domain. It may never ERASE one
	// it was handed.
	//
	// Every resolver falls back to `Sim{At: e.clock.Now(ctx)}` — empty clock
	// id — when it cannot resolve the pin (dangling sub, deleted clock, an
	// unpinned customer). Those fallbacks return a NIL error, so without this
	// guard we would bind them. Under an already-bound ctx that is actively
	// destructive, because `clock.Now(ctx)` READS this binding: the fallback
	// hands back the SIMULATED instant with no clock attached. Binding it
	// keeps the in-simulation instant and drops the clock — SimOf() goes
	// false, and every audit row emitted below this call silently loses its
	// sim columns for the rest of the service call.
	//
	// That is the mirror image of the half-truth Sim exists to prevent. The
	// resolvers' own comments warn about "clock C at wall-clock now"; this is
	// "no clock, at simulated now", and it is the one the code could actually
	// produce. Inside a catchup every entity being touched IS the clock's, so
	// an unresolvable pin there is a lookup failure, never evidence that the
	// work left the simulation.
	if sim.TestClockID == "" {
		if _, inherited := SimOf(ctx); inherited {
			return ctx, true
		}
	}
	// Bind at the boundary, inherit below it (ADR-029). On a test clock the
	// resolver can only answer the clock's frozen_time, so a nested
	// resolution of the SAME clock never knows more than the caller did. A
	// caller that bound an earlier instant on that clock did it on purpose:
	// a cycle close at its boundary, a dunning retry at its slot, a trial
	// flip at trial_end_at, a pause resume at resumes_at. Replacing that
	// instant with frozen_time dated every effect below the rebind (an audit
	// row, a pause start, a write-off) at the advance's target instead of
	// the contracted instant. An inherited instant AFTER frozen_time is not
	// one any caller can contract, so the resolution replaces it, and a
	// different clock always wins.
	if inherited, ok := SimOf(ctx); ok && inherited.TestClockID == sim.TestClockID && !inherited.At.After(sim.At) {
		return ctx, true
	}
	return WithSim(ctx, sim), true
}
