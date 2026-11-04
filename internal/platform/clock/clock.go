// Package clock gives code the current time. A ctx may carry a bound
// instant, such as a test clock's frozen_time, and Now then returns that
// instant instead of the system time.
package clock

import (
	"context"
	"sync"
	"time"
)

// Clock provides the current time. Through ctx, a caller can carry a
// request-scoped effective-now: the instant the request treats as now, such
// as a test clock's frozen_time. Operator entry points bind it with
// WithEffectiveNow. Without one, callers get wall-clock time.
//
// Now takes ctx because, on an entity pinned to a test clock, every
// business timestamp must be in that clock's frozen_time domain (Stripe's
// "no semantic change" guarantee). A bare wall-clock Now() would make every
// call site remember to resolve the pin by hand (see ADR-029). With
// Now(ctx), a service binds effective-now once at the operator entry point,
// and every call below it, stores included, inherits it.
//
// Code that wants wall-clock time whatever the pin calls time.Now().UTC()
// directly, by design. Examples are the cron tick scheduler, webhook
// delivery timestamps, postgres updated_at columns and the audit log's
// recorded_at.
type Clock interface {
	// Now returns ctx's effective-now if one is bound
	// (clock.WithEffectiveNow), and otherwise wall-clock time in UTC. Passing
	// the binding down through ctx means no call site has to remember to
	// resolve the pin.
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

// Sim is the simulated-time context of a code path: the test clock whose
// world it acts in (TestClockID) and the simulated instant it acts at (At).
// The two halves travel together and are set only from one read of the
// clock. A pair like "clock C, at wall-clock now" would be false, and the
// audit log it would be stamped into is append-only. So the type keeps the
// pair together, and no caller has to remember to.
//
// TestClockID == "" means the path is in no simulation. At is then
// wall-clock time, or a bare WithEffectiveNow binding whose clock is not
// known. Nothing is then stamped on the sim axis (the audit_log columns
// sim_effective_at and test_clock_id).
type Sim struct {
	At          time.Time
	TestClockID string
}

// Simulated reports whether this context is inside a test clock's world.
// It needs both halves. A clock id without an instant, or an instant
// without a clock, is a partial binding, and every consumer treats it as
// absent. Otherwise a half-stamped audit row would land in the partial
// clock index and answer sim-time queries with a wall-clock timestamp.
func (s Sim) Simulated() bool {
	return s.TestClockID != "" && !s.At.IsZero()
}

// simKey is the ctx key for the bound Sim. Its type is unexported, so other
// packages can set the binding only through WithSim or WithEffectiveNow.
type simKeyType struct{}

var simKey = simKeyType{}

// WithSim binds a full simulated-time context, instant and clock, to ctx.
// Call it where the clock is known. The callers are the ForClock and
// catch-up drivers, which hold the clock id and its frozen_time, and
// BindEffectiveNow, which resolves an entity's pin. (Catch-up is the billing
// for the time a test clock skipped.) Every Clock.Now(ctx) below reads s.At.
// The audit Logger itself stamps every audit row on this ctx onto the sim
// axis (ADR-090 §5).
//
// The contract is the one ADR-029 set for time: "binding at the boundary,
// inheritance below it." So a new emitter does not have to remember to stamp
// the sim axis.
func WithSim(ctx context.Context, s Sim) context.Context {
	s.At = s.At.UTC()
	return context.WithValue(ctx, simKey, s)
}

// SimOf returns the bound simulated-time context. ok is true only for a
// complete binding on a known test clock (see Sim.Simulated). A bare
// WithEffectiveNow binding reports false: its clock is unknown, and
// inventing one would put false evidence in an append-only log.
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
// Every Clock.Now(ctx) below reads this value instead of wall-clock time.
//
// It clears any clock id already bound on ctx, on purpose. A bare instant
// means this path's time no longer comes from a clock it can name. Keeping
// the old clock id would stamp rows with a clock the new instant does not
// belong to. Callers that know the clock call WithSim.
func WithEffectiveNow(ctx context.Context, t time.Time) context.Context {
	return WithSim(ctx, Sim{At: t})
}

// EffectiveNow returns the bound instant and true when ctx carries any
// binding, from WithEffectiveNow or WithSim. Otherwise it returns the zero
// time and false.
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

// Resolver maps an entity to its simulated-time context. That is the clock
// the entity is pinned to and the clock's frozen_time, or a bare wall-clock
// Sim (TestClockID "") when the entity is not pinned. Operator entry points
// use it to bind ctx through BindEffectiveNow. So do boundary code paths
// with no inherited binding, such as Stripe webhook handlers and async
// workers.
//
// It returns the instant and the clock id together, from one resolution,
// because they are two halves of one fact. A clock id resolved apart from
// its instant can stamp an audit row that claims "clock C at wall-clock
// now" (ADR-090 §5).
//
// *billing.Engine implements it. The clock package owns the interface to
// avoid an import cycle: every domain that needs a binding can depend on
// clock without depending on billing.
type Resolver interface {
	SimForCustomer(ctx context.Context, tenantID, customerID string) (Sim, error)
	SimForSubscription(ctx context.Context, tenantID, subscriptionID string) (Sim, error)
	SimForInvoice(ctx context.Context, tenantID, invoiceID string) (Sim, error)
}

// Pin names the entity to resolve effective-now from. Exactly one of
// CustomerID, SubscriptionID and InvoiceID must be set, and callers pick the
// most specific id they have. TenantID is always required.
type Pin struct {
	TenantID       string
	CustomerID     string
	SubscriptionID string
	InvoiceID      string
}

// BindEffectiveNow resolves p through r and returns a ctx carrying that
// entity's simulated-time context: its effective-now and, when the entity is
// pinned to a test clock, the clock id. On a resolver error it returns the
// original ctx unchanged. Failing the operator's action on a dangling pin is
// worse than stamping the wrong time domain on an edge-case row. The caller
// can log the error if they care. Most don't.
//
// It binds the clock id as well as the instant, so every audit emitter below
// a pin resolution inherits the clock, and every audit row is stamped on the
// sim axis. No emitter has to know that test clocks exist. The name covers
// both, because the clock id is part of the entity's time domain.
//
// The returned bool is true when the ctx now carries effective-now, and
// false when the binding was skipped: no resolver, no ids, or an error.
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
	// A nested resolution may refine the time domain, but never erase one it
	// was handed. A resolver that cannot resolve the pin (a dangling
	// subscription, a deleted clock, an unpinned customer) falls back to
	// `Sim{At: e.clock.Now(ctx)}`, with no clock id and a nil error. Under a
	// bound ctx, `clock.Now(ctx)` reads this binding, so the fallback is the
	// simulated instant with no clock. Binding it would drop the clock:
	// SimOf() would turn false, and every audit row below this call would
	// lose its sim columns for the rest of the service call. That is "no
	// clock, at simulated now", the mirror of the "clock C at wall-clock now"
	// half-truth that Sim exists to prevent. Inside a catch-up every entity
	// touched is the clock's, so an unresolved pin there is a failed lookup,
	// never a sign that the work left the simulation.
	if sim.TestClockID == "" {
		if _, inherited := SimOf(ctx); inherited {
			return ctx, true
		}
	}
	// Bind at the boundary, inherit below it (ADR-029). On a test clock the
	// resolver can only answer the clock's frozen_time, so a nested
	// resolution of the same clock never knows more than the caller did. A
	// caller that bound an earlier instant on that clock did it on purpose:
	// a cycle close at its boundary, a dunning retry at its slot, a trial
	// flip at trial_end_at, a pause resume at resumes_at. Replacing that
	// instant with frozen_time would date every effect below the rebind (an
	// audit row, a pause start, a write-off) at the advance's target, not at
	// the instant the caller set. No caller can set an inherited instant
	// after frozen_time on purpose, so the resolution replaces it. A
	// different clock always wins.
	if inherited, ok := SimOf(ctx); ok && inherited.TestClockID == sim.TestClockID && !inherited.At.After(sim.At) {
		return ctx, true
	}
	return WithSim(ctx, sim), true
}
