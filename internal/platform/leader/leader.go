// Package leader holds singleton-role leadership as a row, not as a
// Postgres session (ADR-114). A singleton role is a background loop, such as
// billing, that one process at a time may run. Each role has one row in
// leader_leases. Four single, self-contained statements run entirely on the
// database clock: acquire, renew, release, and the operator's pause and
// unpause. A fencing token is a number that rises with every acquire. Every
// leader-only claim statement proves it in its own snapshot, with
// leader_fence(role, token). A heartbeat renews the lease during long ticks
// and cancels the work when the lease is lost.
//
// A session-scoped advisory lock lives as long as its TCP session, which can
// outlive its host (#797). It cannot be fenced, so a superseded leader keeps
// issuing statements, and it strands behind transaction-mode poolers. A row
// on the database clock has none of these problems.
//
// Nothing here compares an app timestamp to a database timestamp. Durations
// are measured against durations on the monotonic clock, and every database
// predicate compares now() with values written by now().
package leader

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"
)

// Role names one singleton loop. The list is pinned to migration 0174's
// CHECK constraint by TestLease_RolesSeededMatchGoConstants.
type Role string

const (
	RoleBilling         Role = "billing"
	RoleDunning         Role = "dunning"
	RoleWebhookOutbox   Role = "webhook_outbox"
	RoleEmailOutbox     Role = "email_outbox"
	RoleWebhookDelivery Role = "webhook_delivery"
)

// Roles is every role a binary may lead.
var Roles = []Role{RoleBilling, RoleDunning, RoleWebhookOutbox, RoleEmailOutbox, RoleWebhookDelivery}

// The lease timings are compiled in, like the tick interval. A mis-set TTL
// would mean two leaders at once with no error, so it is not a setting.
// TestLease_ConstantsLeaveNoElectionGap pins AbandonAfter +
// StatementTimeout < LeaseTTL, so a holder that abandons itself stops
// issuing statements before any successor can be elected.
const (
	// LeaseTTL is how long a lease lives without a renew, on the DB clock.
	LeaseTTL = 10 * time.Second
	// HeartbeatEvery is the renew cadence while a tick runs.
	HeartbeatEvery = 3 * time.Second
	// StatementTimeout bounds every leadership statement.
	StatementTimeout = 2 * time.Second
	// AbandonAfter is how long a holder goes without an acknowledged renew
	// before it cancels its own work: two missed beats. It is measured on
	// the monotonic clock from when the last acknowledged renew was sent.
	AbandonAfter = 6 * time.Second
	// MaxPoll caps the time between a follower's polls for a role, so a dead
	// leader's role runs elsewhere within LeaseTTL + MaxPoll.
	MaxPoll = 5 * time.Second
	// interruptedCooldown is how long a tick whose lease was lost (taken over,
	// or its heartbeat timed out) waits before it is due again, or its
	// interval if that is shorter (sqlRelease). The wait keeps a slow database
	// from turning an hourly tick into a partial tick every few seconds. A
	// SIGTERM leaves the cadence alone, even if the work then panicked. A
	// panicked tick waits one full interval (sqlRelease).
	interruptedCooldown = 60 * time.Second
)

var (
	// ErrLeaseLost is the cancellation cause of a tick whose lease was
	// taken over, paused, released, or could not be renewed in time.
	ErrLeaseLost = errors.New("leader: lease lost")
	// ErrNoToken is returned by Fence when a leader-only store method is
	// reached outside a led tick. No query runs, and there is no silent
	// fallback to an unfenced write.
	ErrNoToken = errors.New("leader: ctx carries no lease token — leader-only store method reached outside a led tick")
	// ErrWrongRole is returned by Fence when the ctx was led for a
	// different role than the one the store method belongs to.
	ErrWrongRole = errors.New("leader: ctx lease token is for a different role")
)

// Gate is what scheduler.Run takes. Each Lead call is one attempt to run one
// tick of a role.
type Gate interface {
	// Lead returns (false, nil) when the role is not due, is held elsewhere,
	// or is paused, and the caller polls again. When it leads, work runs on a
	// ctx that carries (role, token) and is cancelled with cause ErrLeaseLost
	// if the heartbeat cannot keep the lease. Lead releases the lease after
	// work returns, on a background ctx, so a cancelled parent cannot skip it.
	// If work panics, Lead releases the tick as not completed and lets the
	// panic continue to the caller, which owns the recover.
	Lead(ctx context.Context, role Role, interval time.Duration, work func(ctx context.Context)) (led bool, err error)
}

type tokenKey struct{}

// tokens is what a led ctx carries: role → fence token. In production a ctx
// holds exactly one entry (Manager.Lead). leadertest may attach several, so
// an end-to-end test can drive two roles' leader-only store methods on one
// ctx.
type tokens map[Role]int64

// WithToken returns ctx carrying the fence token for role, added to any
// tokens the parent already carries.
func WithToken(ctx context.Context, role Role, token int64) context.Context {
	next := tokens{}
	if prev, ok := ctx.Value(tokenKey{}).(tokens); ok {
		for r, t := range prev {
			next[r] = t
		}
	}
	next[role] = token
	return context.WithValue(ctx, tokenKey{}, next)
}

// Fence returns the token that a leader-only store method must prove for
// role want in its claim statement. It returns ErrNoToken when ctx was never
// led, and ErrWrongRole when ctx was led for a different role. A billing
// tick that reaches a dunning store method is a wiring bug, not a follower,
// and must not pass on the strength of the wrong lease.
func Fence(ctx context.Context, want Role) (int64, error) {
	v, ok := ctx.Value(tokenKey{}).(tokens)
	if !ok || len(v) == 0 {
		return 0, ErrNoToken
	}
	token, ok := v[want]
	if !ok || token <= 0 {
		return 0, fmt.Errorf("%w: want %s", ErrWrongRole, want)
	}
	return token, nil
}

// holderID is minted once per process: hostname:pid:8hex. Containers all run
// as pid 1, and StatefulSet pods reuse hostnames, so the random suffix is
// what makes it unique. It is for observability only and is never part of
// the fence.
func holderID() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s:%d:%s", host, os.Getpid(), hex.EncodeToString(b[:]))
}
