package subscription

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// cancelCameDue is a store whose activation finds a cancel schedule came due
// after the list (ErrTrialCancelDue), and whose re-read of the subscription
// after that fails.
type cancelCameDue struct {
	*memStore
	refused bool
	reread  error
}

func (s *cancelCameDue) ActivateAfterTrialWithBill(context.Context, string, string, time.Time, func(*sql.Tx, domain.Subscription) error) (domain.Subscription, error) {
	s.refused = true
	return domain.Subscription{}, domain.ErrTrialCancelDue
}

func (s *cancelCameDue) Get(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	if s.refused {
		return domain.Subscription{}, s.reread
	}
	return s.memStore.Get(ctx, tenantID, id)
}

// When a trial's activation finds a cancel came due, the job re-reads the
// subscription to cancel it free. A failed re-read is an error of the job,
// in both of them: on a clock it fails the advance, which would otherwise
// end ready with the trial neither cancelled nor activated; in real time it
// is logged as an error, and the next tick tries again.
func TestTrialEnd_FailedRereadAfterTheCancelCameDueIsAnError(t *testing.T) {
	createdAt := time.Date(2025, 11, 29, 12, 0, 0, 0, time.UTC)
	reread := errors.New("re-read failed")
	setup := func(t *testing.T, ctx context.Context) (*memStore, domain.Subscription) {
		t.Helper()
		svc := NewService(newMemStore(), clock.NewFake(createdAt))
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "sub-trial-reread", DisplayName: "Trial", CustomerID: "c",
			Items: []CreateItemInput{{PlanID: "p"}}, TrialDays: 14,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		return svc.store.(*memStore), sub
	}
	check := func(t *testing.T, errs []error, mem *memStore, id string) {
		t.Helper()
		if len(errs) != 1 || !errors.Is(errs[0], reread) {
			t.Fatalf("errors = %v, want the failed re-read", errs)
		}
		if got := mem.subs[id].Status; got != domain.SubscriptionTrialing {
			t.Errorf("status = %s, want still trialing", got)
		}
	}

	t.Run("on a clock", func(t *testing.T) {
		mem, sub := setup(t, context.Background())
		pinned := mem.subs[sub.ID]
		pinned.TestClockID = "tclk_1"
		mem.subs[sub.ID] = pinned
		frozen := sub.TrialEndAt.Add(24 * time.Hour)
		ctx := clock.WithSim(context.Background(), clock.Sim{At: frozen, TestClockID: "tclk_1"})
		svc := NewService(&cancelCameDue{memStore: mem, reread: reread}, clock.NewFake(frozen))

		_, errs := svc.ProcessExpiredTrialsForClock(ctx, "t1", "tclk_1", frozen)
		check(t, errs, mem, sub.ID)
	})

	t.Run("in real time", func(t *testing.T) {
		ctx := postgres.WithLivemode(context.Background(), true)
		mem, sub := setup(t, ctx)
		svc := NewService(&cancelCameDue{memStore: mem, reread: reread}, clock.NewFake(sub.TrialEndAt.Add(24*time.Hour)))

		_, errs := svc.ProcessExpiredTrials(ctx, 100)
		check(t, errs, mem, sub.ID)
	})
}
