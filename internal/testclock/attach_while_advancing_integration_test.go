package testclock_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/leader/leadertest"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testclock"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestCustomerJoinsAClock_RefusedWhileItAdvances pins what
// domain.TestClockStatus says: a request that creates a customer on an
// advancing clock is refused (409 clock_advancing), and on a failed clock the
// customer joins at the new frozen_time the advance already stamped.
func TestCustomerJoinsAClock_RefusedWhileItAdvances(t *testing.T) {
	db := testutil.SetupTestDB(t)
	clocks := testclock.NewPostgresStore(db)
	// The lease lets the internal_failure case park the clock with the
	// catch-up's fenced MarkFailed.
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleTestClockCatchup)
	tenantID := testutil.CreateTestTenant(t, db, "Attach in any status")
	customers := customer.NewService(customer.NewPostgresStore(db))
	customers.SetTestClockChecker(clocks)

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	// The request is marked as a change, as the API's door marks it.
	request := clock.MarkChange(ctx)
	for _, c := range []struct {
		name    string
		park    func(t *testing.T, id string)
		want    domain.TestClockStatus
		refused bool
	}{
		{"advancing", func(t *testing.T, id string) {
			if _, err := clocks.MarkAdvancing(ctx, tenantID, id, to, noAuditRow); err != nil {
				t.Fatalf("mark advancing: %v", err)
			}
		}, domain.TestClockStatusAdvancing, true},
		{"internal_failure", func(t *testing.T, id string) {
			if _, err := clocks.MarkAdvancing(ctx, tenantID, id, to, noAuditRow); err != nil {
				t.Fatalf("mark advancing: %v", err)
			}
			if _, err := clocks.MarkFailed(ctx, tenantID, id, "catch-up failed", domain.AdvanceSummary{}); err != nil {
				t.Fatalf("mark failed: %v", err)
			}
		}, domain.TestClockStatusInternalFailed, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			clk, err := clocks.Create(ctx, tenantID, domain.TestClock{Name: c.name, FrozenTime: start}, noAuditRow)
			if err != nil {
				t.Fatalf("create clock: %v", err)
			}
			c.park(t, clk.ID)
			if got, err := clocks.Get(ctx, tenantID, clk.ID); err != nil || got.Status != c.want {
				t.Fatalf("clock status %q (err %v), want %q", got.Status, err, c.want)
			}

			cust, err := customers.Create(request, tenantID, customer.CreateInput{
				ExternalID: "cus_on_" + c.name, DisplayName: "On " + c.name, TestClockID: clk.ID,
			})
			if c.refused {
				if !clock.IsAdvancingRefusal(err) {
					t.Fatalf("create a customer on an advancing clock: %v, want 409 clock_advancing", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("create a customer on a %s clock: %v", c.want, err)
			}
			if cust.TestClockID != clk.ID || !cust.CreatedAt.Equal(to) {
				t.Fatalf("customer on %q created %v, want %q at the new frozen_time %v", cust.TestClockID, cust.CreatedAt, clk.ID, to)
			}
		})
	}
}
