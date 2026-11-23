package api

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/leader/leadertest"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// tickAt sets the server's clock to at, then runs the billing and dunning
// jobs that are due for tenantID, as their leaders would at that instant.
func tickAt(t *testing.T, srv *Server, wall *clock.Fake, tenantID string, at time.Time) {
	t.Helper()
	wall.Set(at)
	ctx := clock.WithRealTime(postgres.WithLivemode(context.Background(), false))
	admin := testutil.AdminPool(t)
	if _, errs := srv.BillingEngine.RunCycle(leadertest.Token(t, admin, ctx, leader.RoleBilling), 50, nil); len(errs) != 0 {
		t.Fatalf("billing tick at %v: %v", at, errs)
	}
	if _, errs := srv.DunningSvc.ProcessDueRuns(leadertest.Token(t, admin, ctx, leader.RoleDunning), tenantID, 50); len(errs) != 0 {
		t.Fatalf("dunning tick at %v: %v", at, errs)
	}
}
