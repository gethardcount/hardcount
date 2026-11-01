package testclock_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testclock"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestCustomerJoinsAClockInAnyStatus pins what domain.TestClockStatus says:
// no clock status blocks attaching a customer. On an advancing clock and on a
// failed one, a customer is created on the clock, and the clock's instant it
// joins at is the new frozen_time the advance already stamped. The comment
// used to say a failed clock blocks attachment; nothing ever enforced it.
// Mutation-verify: refuse a non-ready clock in ResolveSim → both cases fail.
func TestCustomerJoinsAClockInAnyStatus(t *testing.T) {
	db := testutil.SetupTestDB(t)
	clocks := testclock.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Attach in any status")
	customers := customer.NewService(customer.NewPostgresStore(db))
	customers.SetTestClockChecker(clocks)

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		park func(t *testing.T, id string)
		want domain.TestClockStatus
	}{
		{"advancing", func(t *testing.T, id string) {
			if _, err := clocks.MarkAdvancing(ctx, tenantID, id, to); err != nil {
				t.Fatalf("mark advancing: %v", err)
			}
		}, domain.TestClockStatusAdvancing},
		{"internal_failure", func(t *testing.T, id string) {
			if _, err := clocks.MarkAdvancing(ctx, tenantID, id, to); err != nil {
				t.Fatalf("mark advancing: %v", err)
			}
			if _, err := clocks.MarkFailed(ctx, tenantID, id, "catch-up failed"); err != nil {
				t.Fatalf("mark failed: %v", err)
			}
		}, domain.TestClockStatusInternalFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			clk, err := clocks.Create(ctx, tenantID, domain.TestClock{Name: c.name, FrozenTime: start})
			if err != nil {
				t.Fatalf("create clock: %v", err)
			}
			c.park(t, clk.ID)
			if got, err := clocks.Get(ctx, tenantID, clk.ID); err != nil || got.Status != c.want {
				t.Fatalf("clock status %q (err %v), want %q", got.Status, err, c.want)
			}

			sim, ok, err := clocks.ResolveSim(ctx, tenantID, clk.ID)
			if err != nil || !ok {
				t.Fatalf("ResolveSim on a %s clock = ok %v, err %v; want attach-able", c.want, ok, err)
			}
			if !sim.At.Equal(to) || sim.TestClockID != clk.ID {
				t.Fatalf("ResolveSim = %v on %s, want the new frozen_time %v on %s", sim.At, sim.TestClockID, to, clk.ID)
			}

			cust, err := customers.Create(ctx, tenantID, customer.CreateInput{
				ExternalID: "cus_on_" + c.name, DisplayName: "On " + c.name, TestClockID: clk.ID,
			})
			if err != nil {
				t.Fatalf("create a customer on a %s clock: %v", c.want, err)
			}
			if cust.TestClockID != clk.ID {
				t.Fatalf("customer pinned to %q, want %q", cust.TestClockID, clk.ID)
			}
		})
	}
}
