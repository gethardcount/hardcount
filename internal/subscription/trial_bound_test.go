package subscription

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// TestExtendTrial_TwoYearBound: create refuses trial_days above 730, but
// extend-trial took any later trial_end, so a trial could run for years.
// Both writers of trial_end_at now hold one rule: a trial ends at most 730
// days after it started. Exactly 730 days is accepted.
func TestExtendTrial_TwoYearBound(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	svc := NewService(newMemStore(), clock.NewFake(now))
	sub, err := svc.Create(context.Background(), "t1", CreateInput{
		Code: "sub-trial-bound", DisplayName: "Trial", CustomerID: "c",
		Items:     []CreateItemInput{{PlanID: "p"}},
		TrialDays: 14,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	limit := sub.TrialStartAt.AddDate(0, 0, maxTrialDays)

	_, err = svc.ExtendTrial(context.Background(), "t1", sub.ID, limit.Add(24*time.Hour))
	if !errors.Is(err, errs.ErrValidation) || errs.Field(err) != "trial_end" {
		t.Fatalf("731 days: got %v, want a trial_end validation error", err)
	}

	out, err := svc.ExtendTrial(context.Background(), "t1", sub.ID, limit)
	if err != nil {
		t.Fatalf("730 days: %v", err)
	}
	if !out.TrialEndAt.Equal(limit) {
		t.Errorf("trial_end_at: got %v, want %v", out.TrialEndAt, limit)
	}
}
