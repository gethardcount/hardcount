// Package breaker provides a global circuit breaker around Stripe API
// calls, built on sony/gobreaker/v2. Stripe is a single external
// dependency. When it is unhealthy (5xx, timeout, connection reset), every
// caller should back off together, so Hardcount does not pile on during an
// incident.
//
// Only "unknown" outcomes count as failures: a 5xx, a timeout or a
// connection reset, the problems on Stripe's side. Card declines,
// validation errors and calls the breaker rejected are left out of its
// counts, so a batch of expired cards does not trip the breaker.
//
// The breaker does not isolate tenants. One tenant's Stripe account that
// keeps answering 5xx is a rare case, which can be split off later if it
// happens. Per-tenant breakers cost a state machine per tenant, metric
// cardinality and admin endpoints. That is not worth paying for a case
// that has not shown up in production.
package breaker

import (
	"context"
	"errors"
	"sync"
	"time"

	gb "github.com/sony/gobreaker/v2"
)

// State is a human-readable breaker state. It is emitted as a metric label.
type State string

const (
	StateClosed   State = "closed"
	StateOpen     State = "open"
	StateHalfOpen State = "half_open"
)

func fromGB(s gb.State) State {
	switch s {
	case gb.StateClosed:
		return StateClosed
	case gb.StateHalfOpen:
		return StateHalfOpen
	case gb.StateOpen:
		return StateOpen
	}
	return StateClosed
}

// ErrOpen is returned when the breaker is open, or half-open and at its
// probe limit. Callers should treat it as a transient, retryable failure.
// The Stripe call never happened, so do not mark the invoice failed, and do
// not count it as a dunning attempt (a payment retry).
//
// One sentinel stands for both of gobreaker's ErrOpenState and
// ErrTooManyRequests, because in practice the two mean the same thing: "the
// breaker is protecting Stripe right now, try later."
var ErrOpen = errors.New("stripe circuit breaker open")

// Countable reports whether an error returned from a Stripe call should
// count as a breaker failure. The canonical implementation in package
// payment returns true for PaymentError.Unknown (5xx, timeout), and false
// for card declines and validation errors.
//
// An implementation must never return true for nil. A nil error is always
// a success, and gobreaker handles that itself.
type Countable func(err error) bool

// Config tunes the breaker. The defaults target Stripe's published error
// rates. At 5 consecutive Unknown failures, an incident is almost certain,
// not bad luck, and rapid retries only widen the damage.
type Config struct {
	// FailureThreshold is the number of consecutive Unknown failures that
	// will open the breaker. Defaults to 5.
	FailureThreshold int
	// Cooldown is how long the breaker stays open before allowing a probe.
	// Defaults to 30s.
	Cooldown time.Duration
	// Interval clears the internal Counts periodically in the closed state,
	// so isolated failures spaced far apart do not add up toward the
	// threshold. 0 turns this off, and the counts then last until a success
	// resets them. Defaults to 60s.
	Interval time.Duration
	// Countable classifies errors returned from Stripe. Required.
	Countable Countable
	// OnStateChange fires whenever the breaker transitions states.
	OnStateChange func(from, to State)
}

// Breaker is a thread-safe global circuit breaker.
type Breaker struct {
	cb *gb.CircuitBreaker[any]
	mu sync.Mutex
}

// New constructs a Breaker. cfg.Countable is required. Zero values in the
// other fields are replaced with defaults.
func New(cfg Config) *Breaker {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 5
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 30 * time.Second
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 60 * time.Second
	}
	if cfg.Countable == nil {
		// The safer default treats every error as a failure. Production
		// wiring always supplies a real classifier. This default exists
		// only to avoid a nil dereference if someone forgets.
		cfg.Countable = func(err error) bool { return err != nil }
	}

	threshold := cfg.FailureThreshold
	countable := cfg.Countable
	onChange := cfg.OnStateChange

	cb := gb.NewCircuitBreaker[any](gb.Settings{
		Name:        "stripe",
		MaxRequests: 1, // one half-open probe at a time
		Interval:    cfg.Interval,
		Timeout:     cfg.Cooldown,
		ReadyToTrip: func(c gb.Counts) bool {
			return int(c.ConsecutiveFailures) >= threshold
		},
		// With the default IsSuccessful, a nil error is a success and a
		// non-nil error is a failure. IsExcluded keeps card declines, a
		// customer problem and not a Stripe problem, out of the failure
		// count. gobreaker checks IsExcluded before IsSuccessful, so an
		// excluded error is not counted as a success either.
		IsExcluded: func(err error) bool {
			if err == nil {
				return false
			}
			return !countable(err)
		},
		OnStateChange: func(_ string, from, to gb.State) {
			if onChange != nil {
				onChange(fromGB(from), fromGB(to))
			}
		},
	})

	return &Breaker{cb: cb}
}

// Execute runs fn under the breaker. It returns ErrOpen if the breaker is
// rejecting calls, and otherwise fn's (any, error). Only an error for which
// cfg.Countable returns true counts toward the breaker's failure count. A
// card decline returns an error but is left out of the count.
func (b *Breaker) Execute(ctx context.Context, fn func(context.Context) (any, error)) (any, error) {
	result, err := b.cb.Execute(func() (any, error) {
		return fn(ctx)
	})
	if errors.Is(err, gb.ErrOpenState) || errors.Is(err, gb.ErrTooManyRequests) {
		return nil, ErrOpen
	}
	return result, err
}

// State returns the current breaker state, for metrics. Do not gate calls
// on it, because it races with a concurrent Execute. Always call Execute
// and handle ErrOpen.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return fromGB(b.cb.State())
}
