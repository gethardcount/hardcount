package subscription

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// A 14-day trial created 2026-03-01 23:30 EST in an America/New_York tenant
// ends 2026-03-15 23:30 EDT, and the first paid period starts Mar 15. Pre-fix
// the trial end was 14×24h on the UTC instant: 2026-03-16 00:30 EDT, one hour
// late because DST starts on Mar 8, so the first paid period started Mar 16.
func TestCreate_TrialDays_UsesTenantCalendarAcrossDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	created := time.Date(2026, 3, 2, 4, 30, 0, 0, time.UTC)         // Mar 1 23:30 EST
	wantTrialEnd := time.Date(2026, 3, 16, 3, 30, 0, 0, time.UTC)   // Mar 15 23:30 EDT
	wantPeriodStart := time.Date(2026, 3, 15, 4, 0, 0, 0, time.UTC) // Mar 15 00:00 EDT

	svc := NewService(newMemStore(), clock.NewFake(created))
	svc.SetSettingsReader(fakeSettings{tz: "America/New_York"})

	sub, err := svc.Create(context.Background(), "t1", CreateInput{
		Code: "sub-trial-dst", DisplayName: "Trial DST",
		CustomerID: "cus_1",
		Items:      []CreateItemInput{{PlanID: "pln_1"}},
		TrialDays:  14,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sub.TrialEndAt == nil || !sub.TrialEndAt.Equal(wantTrialEnd) {
		t.Errorf("trial_end_at = %v, want %s (Mar 15 23:30 EDT)", sub.TrialEndAt, wantTrialEnd.Format(time.RFC3339))
	}
	if sub.CurrentBillingPeriodStart == nil || !sub.CurrentBillingPeriodStart.Equal(wantPeriodStart) {
		t.Fatalf("first paid period start = %v, want %s (Mar 15 00:00 EDT)", sub.CurrentBillingPeriodStart, wantPeriodStart.Format(time.RFC3339))
	}
	if got := sub.CurrentBillingPeriodStart.In(ny).Format("Jan 2, 2006"); got != "Mar 15, 2026" {
		t.Errorf("first paid period starts %q, want %q", got, "Mar 15, 2026")
	}
}
