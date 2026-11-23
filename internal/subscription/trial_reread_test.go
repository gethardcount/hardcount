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
// subscription to cancel it free. A failed re-read is an error of the job:
// it is logged as an error, and the next tick tries again.
func TestTrialEnd_FailedRereadAfterTheCancelCameDueIsAnError(t *testing.T) {
	createdAt := time.Date(2025, 11, 29, 12, 0, 0, 0, time.UTC)
	reread := errors.New("re-read failed")
	ctx := postgres.WithLivemode(context.Background(), true)
	mem := newMemStore()
	sub, err := NewService(mem, clock.NewFake(createdAt)).Create(ctx, "t1", CreateInput{
		Code: "sub-trial-reread", DisplayName: "Trial", CustomerID: "c",
		Items: []CreateItemInput{{PlanID: "p"}}, TrialDays: 14,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	svc := NewService(&cancelCameDue{memStore: mem, reread: reread}, clock.NewFake(sub.TrialEndAt.Add(24*time.Hour)))

	_, errs := svc.ProcessExpiredTrials(ctx, 100)
	if len(errs) != 1 || !errors.Is(errs[0], reread) {
		t.Fatalf("errors = %v, want the failed re-read", errs)
	}
	if got := mem.subs[sub.ID].Status; got != domain.SubscriptionTrialing {
		t.Errorf("status = %s, want still trialing", got)
	}
}
