package testutil

import (
	"context"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// MustLivemode returns the mode on ctx, for test fakes that record it. It
// panics when ctx has none, so a missing postgres.WithLivemode fails the test.
func MustLivemode(ctx context.Context) bool {
	live, err := postgres.Livemode(ctx)
	if err != nil {
		panic(err)
	}
	return live
}
