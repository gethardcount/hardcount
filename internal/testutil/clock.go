package testutil

import (
	"context"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// AtRealTime is a test-mode context with no instant bound: where a job's tick
// starts (the scheduler drops any instant, so each row binds its own).
func AtRealTime() context.Context {
	return clock.WithRealTime(postgres.WithLivemode(context.Background(), false))
}
