package testutil

import (
	"context"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// AtRealTime is a test-mode context decided at real time with no instant:
// where a job's tick starts (the scheduler decides real time, and each row
// binds its own). A read of the server's clock on it fails under tests; work
// that runs on it binds first, or reads a list cutoff (clock.Cutoff).
func AtRealTime() context.Context {
	return clock.WithRealTime(postgres.WithLivemode(context.Background(), false))
}

// RealInstant is ctx bound to one read of the server's clock, as the API's
// door binds a request on a record on no clock: for a test's seeding or an
// operator's request that means a real instant.
func RealInstant(ctx context.Context) context.Context {
	return clock.EnsureEffectiveNow(clock.WithRealTime(ctx), clock.Real())
}

// DecidedFor is a test-mode context decided from the record p names, as the
// API's door decides a request on it: the record's clock, or real time. A
// test is an entry point, so it decides where production would. It fails the
// test when the time cannot be decided.
//
// Both helpers start a fresh test-mode context. A test that already holds a
// context it needs (a leader token, a timeout) decides on that context with
// clock.WithRealTime or clock.Decide instead.
func DecidedFor(t testing.TB, r clock.Resolver, p clock.Pin) context.Context {
	t.Helper()
	ctx, err := clock.Decide(postgres.WithLivemode(context.Background(), false), r, p)
	if err != nil {
		t.Fatalf("decide the test's time: %v", err)
	}
	return ctx
}
