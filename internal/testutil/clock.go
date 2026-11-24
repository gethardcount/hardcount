package testutil

import (
	"context"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// AtRealTime is a test-mode context decided at real time: where a job's tick
// starts (the scheduler decides real time).
func AtRealTime() context.Context {
	return clock.WithRealTime(postgres.WithLivemode(context.Background(), false))
}

// DecidedFor is a test-mode context decided from the record p names, as a
// function that decides its own time does: the record's clock, or real time.
// A test is an entry point, so it decides where production would. It fails the
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
