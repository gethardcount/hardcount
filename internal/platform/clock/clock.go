// Package clock gives code the current time. A ctx may carry a bound
// instant, such as a test clock's frozen_time, and Now then returns that
// instant instead of the system time.
//
// One piece of work runs on one time. On a real-time or a record route the
// API's door (api.clockDoor) binds the request's instant before the handler
// runs. A function that decides its own time (a route whose body names its
// record, a public page, a Stripe event) calls EnsureEffectiveNow right after
// it decides, with the service's Clock: a clock's instant is kept, and a
// record on no clock gets one read of that Clock bound as a real instant (the
// states are named on Decision). Every read of the clock below then answers
// that instant. A request that starts a job over many records hands the job
// a context with no instant, and the job binds one per row. A fan-out that
// runs after the request has answered, over rows that may each be on their
// own clock, does not bind one instant for all of them: it decides each row,
// and the row's work binds its own. A payment's result is its own event,
// recorded when Stripe's answer arrives (AtArrival), in the same call or by
// webhook, and so is what the action writes on that invoice after it.
package clock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// Clock provides the current time. Through ctx, a caller can carry a
// request-scoped effective-now: the instant the request treats as now, such
// as a test clock's frozen_time. Operator entry points bind it with
// WithEffectiveNow. Without one, callers get wall-clock time.
//
// Now takes ctx because, on an entity pinned to a test clock, every
// business timestamp must be in that clock's frozen_time domain (Stripe's
// "no semantic change" guarantee). A bare wall-clock Now() would make every
// call site remember to resolve the pin by hand: the leak ADR-030 records.
// With Now(ctx), a service binds effective-now once at the operator entry
// point, and every call below it, stores included, inherits it.
//
// Code that wants wall-clock time even on a pinned entity calls
// time.Now().UTC() directly, by design. Examples are the cron tick
// scheduler, webhook delivery timestamps and the audit log's recorded_at.
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
// TestClockID == "" means the path is in no simulation: real time with no
// instant (a zero Sim), or a real instant (At set). Nothing is then stamped on
// the sim axis (the audit_log columns sim_effective_at and test_clock_id).
// The states a context can carry are defined once, on Decision.
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
// It is called where the clock is known: by the ForClock and catch-up
// drivers, which hold the clock id and its frozen_time, and by
// BindEffectiveNow, which resolves an entity's pin. (Catch-up is the work
// that runs everything that fell due in the time a test clock skipped.)
// Every Clock.Now(ctx) below reads s.At.
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
// clock and its instant (see Sim.Simulated). A real instant reports false:
// it names no clock, and inventing one would put false evidence in an
// append-only log.
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

// WithEffectiveNow binds a real instant (At set, no clock id; see Decision).
// Every Clock.Now(ctx) below reads this value instead of wall-clock time.
//
// It clears any clock id already bound on ctx, on purpose: a real instant
// names no clock, and keeping the old clock id would stamp rows with a clock
// the new instant does not belong to. Callers that know the clock call
// WithSim.
func WithEffectiveNow(ctx context.Context, t time.Time) context.Context {
	return WithSim(ctx, Sim{At: t})
}

// EnsureEffectiveNow returns ctx with an effective now bound: the one ctx
// carries (a clock and its instant, or a real instant), else c's now, read
// once and bound as a real instant. A decision of real time may carry no
// instant (WithRealTime), so work that read the clock more than once would
// stamp several instants on one piece of work; binding here gives everything
// below one instant.
func EnsureEffectiveNow(ctx context.Context, c Clock) context.Context {
	if _, ok := EffectiveNow(ctx); ok {
		return ctx
	}
	return WithEffectiveNow(ctx, c.Now(ctx))
}

// AtArrival returns ctx for recording a payment's result, Stripe's answer to a
// charge, and what the action writes on that invoice after it. A Stripe Tax
// answer is not one: it keeps the action's instant. On a test clock ctx is
// kept: its clock and instant. On no clock c is read once, and that read
// replaces any real instant ctx carries, so the result is dated when it
// arrived.
func AtArrival(ctx context.Context, c Clock) context.Context {
	if _, ok := SimOf(ctx); ok {
		return ctx
	}
	return WithEffectiveNow(ctx, c.Now(WithRealTime(ctx)))
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

// WithRealTime records on ctx that real time was decided for it, with no
// instant: it is in no test clock's world. Now(ctx) then reads machine time,
// as on an undecided ctx, but Decision reports the decision. A job's tick, a
// test-mode request on a decidedIn route (until its function decides and
// binds), and an entity on no clock with nothing bound are decided this way;
// for an entity on no clock, a real instant ctx already carries is kept
// instead (keepOrRealTime).
func WithRealTime(ctx context.Context) context.Context {
	return WithSim(ctx, Sim{})
}

// DecidesWithoutDoor reports whether work on a record that the API's door
// did not decide decides its time. On a real-time or a record route the
// door (api.clockDoor) decides and binds the request's time before its
// handler; this is for work the door does not decide: a decidedIn route's
// function, a Stripe event, a job's row, a public page. With no resolver, no: only narrow unit tests build their service
// without one, and the server always wires the engine. With no mode
// on ctx, a fault, refused as ErrUndecided: every such entry point sets one.
// In live mode, no: no live record is on a test clock, and live work pays for
// no lookup. Else yes. A caller that must read the record before it can name
// it asks this first, so live work reads nothing.
func DecidesWithoutDoor(ctx context.Context, r Resolver) (bool, error) {
	if r == nil {
		return false, nil
	}
	live, err := postgres.Livemode(ctx)
	if err != nil {
		return false, fmt.Errorf("%w: the context has no mode", ErrUndecided)
	}
	return !live, nil
}

// DecideWithoutDoor decides the time of work on a record that did not come
// through the API's door (see DecidesWithoutDoor). When DecidesWithoutDoor
// says it decides: the record's clock, or real time for a record on no clock
// (Decide); else ctx unchanged. A failure is ErrUndecided. A request or a
// page that names its record calls DecideOrRealTime, for which a not-found
// means going on at real time; an event or a job's row calls this, for which
// a not-found means the record is not here yet, or is gone.
func DecideWithoutDoor(ctx context.Context, r Resolver, p Pin) (context.Context, error) {
	decides, err := DecidesWithoutDoor(ctx, r)
	if err != nil || !decides {
		return ctx, err
	}
	return Decide(ctx, r, p)
}

// DecideOrRealTime is DecideWithoutDoor for a request or a page that names
// its record. A not-found is the record, or, in a race with its clock's
// deletion, its clock: real time is right in both. Any other failure is
// returned (ErrUndecided over its cause), for the caller to refuse with its
// own message: what it did not record, create or show.
func DecideOrRealTime(ctx context.Context, r Resolver, p Pin) (context.Context, error) {
	decided, err := DecideWithoutDoor(ctx, r, p)
	if errors.Is(err, errs.ErrNotFound) {
		return keepOrRealTime(ctx), nil
	}
	return decided, err
}

// keepOrRealTime is the decision for a record on no clock. An instant with no
// clock id (a real instant) is the work's time, read once by whoever bound
// it, and is kept so the work stays on one instant. Anything else becomes
// real time with no instant (WithRealTime): a clock's instant is not kept,
// because the record is on no clock.
func keepOrRealTime(ctx context.Context) context.Context {
	if s, ok := Decision(ctx); ok && s.TestClockID == "" && !s.At.IsZero() {
		return ctx
	}
	return WithRealTime(ctx)
}

// Decision returns what ctx carries for its time. A context carries one of
// four states, named the same everywhere:
//   - nothing decided: no binding (ok is false);
//   - real time with no instant: a zero Sim (WithRealTime);
//   - a real instant: At set, no clock id (WithEffectiveNow,
//     EnsureEffectiveNow);
//   - a clock and its instant: a clock id and At (WithSim).
//
// Unlike SimOf, it tells real time from nothing decided.
func Decision(ctx context.Context) (Sim, bool) {
	if ctx == nil {
		return Sim{}, false
	}
	s, ok := ctx.Value(simKey).(Sim)
	return s, ok
}

// ErrUndecided is Decide's error when the resolver could not read the
// record or its clock. It wraps the resolver's error, so the cause stays
// readable. A caller refuses the work and lets its sender retry (a webhook
// answers 5xx, so Stripe delivers it again). It checks ErrUndecided before
// any not-found test: the cause underneath may itself be a not-found, and a
// record whose time could not be read is not a record that is missing.
var ErrUndecided = errors.New("the time could not be decided")

// Decide resolves p through r and records the answer on ctx. Unlike
// BindEffectiveNow it returns the resolver's error, as ErrUndecided, and
// needs a resolver and an id: it is the call that decides, at the entry point
// of a request or a job's row. A clock is decided as the record's clock. For
// a record on no clock the result is real time, except that a real instant
// ctx already carries is kept; a clock's instant is dropped
// (keepOrRealTime).
// Work on a record that did not come through the API's clock door decides
// with DecideWithoutDoor, which calls it.
func Decide(ctx context.Context, r Resolver, p Pin) (context.Context, error) {
	if r == nil {
		return ctx, errors.New("decide the time: no resolver")
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
		return ctx, errors.New("decide the time: the pin names no customer, subscription or invoice")
	}
	if err != nil {
		return ctx, fmt.Errorf("%w: %w", ErrUndecided, err)
	}
	if !sim.Simulated() {
		return keepOrRealTime(ctx), nil
	}
	return WithSim(ctx, sim), nil
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
// a pin resolution inherits the clock, and no audit row written below it
// misses the sim axis. No emitter has to know that test clocks exist. The
// name still fits: the call binds the entity's time domain onto ctx, and the
// clock id is part of that domain.
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
		// A record the resolver could not read: go on in the time ctx
		// already has. Logged here, the one place every bind passes.
		slog.WarnContext(ctx, "clock: could not resolve the record's time; going on in the time the context has",
			"tenant_id", p.TenantID, "customer_id", p.CustomerID, "subscription_id", p.SubscriptionID,
			"invoice_id", p.InvoiceID, "error", err)
		return ctx, false
	}
	// A nested resolution may refine the time domain, but never erase one it
	// was handed. A record on no clock (an unpinned customer, say) answers
	// with no clock id; under a bound ctx its instant is the bound one, so it
	// is the simulated instant with no clock. Binding it would drop the
	// clock: SimOf would turn false, and every audit row below this call
	// would lose its sim columns for the rest of the service call. Inside a
	// catch-up every record touched is the clock's, so an answer with no
	// clock there is never a sign that the work left the simulation.
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
