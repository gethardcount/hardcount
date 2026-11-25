package subscription

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

var errPlanReadDown = errors.New("connection reset by peer")

// anchorReadFails answers every plan read as fakePlanReader does, except the
// one firstPlanInterval makes to anchor the billing period, which fails. The
// earlier reads of the same request (archived check, currency pin) pass, so
// each test reaches the anchoring read itself.
type anchorReadFails struct {
	fakePlanReader
	err error
}

func (f *anchorReadFails) GetPlan(ctx context.Context, tenantID, planID string) (domain.Plan, error) {
	if f.err != nil && calledFrom("firstPlanInterval") {
		return domain.Plan{}, f.err
	}
	return f.fakePlanReader.GetPlan(ctx, tenantID, planID)
}

// calledFrom reports whether a function whose name ends in fn is on the
// caller's stack. CallersFrames expands inlined frames, so an inlined
// firstPlanInterval is still found.
func calledFrom(fn string) bool {
	pcs := make([]uintptr, 32)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		f, more := frames.Next()
		if strings.HasSuffix(f.Function, "."+fn) {
			return true
		}
		if !more {
			return false
		}
	}
}

// TestFirstPlanInterval_ReadErrorIsAnError pins the shared function (#1449):
// a plan that cannot be read, or has no interval, is an error, never a
// monthly anchor. A missing plan loses errs.ErrNotFound, so it answers 500,
// not a 404 naming the subscription. No reader, or no items, stays monthly.
// Mutation-verify: return BillingMonthly on a read error → this fails.
func TestFirstPlanInterval_ReadErrorIsAnError(t *testing.T) {
	ctx := context.Background()
	items := []domain.SubscriptionItem{{PlanID: "p"}}
	svc := NewService(newMemStore(), nil)

	svc.SetPlanReader(&anchorReadFails{err: errPlanReadDown})
	if iv, err := svc.firstPlanInterval(ctx, "t1", items); !errors.Is(err, errPlanReadDown) {
		t.Errorf("read error: got %q, %v; want the read error", iv, err)
	}
	svc.SetPlanReader(&anchorReadFails{err: errs.ErrNotFound})
	if iv, err := svc.firstPlanInterval(ctx, "t1", items); err == nil || errors.Is(err, errs.ErrNotFound) {
		t.Errorf("missing plan: got %q, %v; want an error that is not ErrNotFound", iv, err)
	}
	svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{"p": {ID: "p"}}})
	if iv, err := svc.firstPlanInterval(ctx, "t1", items); err == nil {
		t.Errorf("plan with no interval: got %q, want an error", iv)
	}
	svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{"p": {ID: "p", BillingInterval: domain.BillingYearly}}})
	if iv, err := svc.firstPlanInterval(ctx, "t1", items); err != nil || iv != domain.BillingYearly {
		t.Errorf("yearly plan: got %q, %v; want yearly", iv, err)
	}
	if iv, err := NewService(newMemStore(), nil).firstPlanInterval(ctx, "t1", items); err != nil || iv != domain.BillingMonthly {
		t.Errorf("no reader: got %q, %v; want monthly", iv, err)
	}
}

// TestAnchoringPlanReadError_EachCallerStops: each caller of
// firstPlanInterval returns the plan read error and writes nothing. Before
// #1449 each anchored the period as monthly and went on: a yearly plan got a
// one-month first period, billed as a month.
// Mutation-verify: in any one caller, drop its `if err != nil` after
// firstPlanInterval → that subtest fails.
func TestAnchoringPlanReadError_EachCallerStops(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2025, 11, 29, 10, 0, 0, 0, time.UTC)
	yearly := map[string]domain.Plan{"p": {ID: "p", BillingInterval: domain.BillingYearly}}

	t.Run("Create", func(t *testing.T) {
		mem := newMemStore()
		svc := NewService(mem, clock.NewFake(now))
		svc.SetPlanReader(&anchorReadFails{fakePlanReader: fakePlanReader{plans: yearly}, err: errPlanReadDown})
		_, err := svc.Create(ctx, "t1", CreateInput{Code: "now", DisplayName: "n", CustomerID: "c",
			Items: []CreateItemInput{{PlanID: "p"}}})
		if !errors.Is(err, errPlanReadDown) {
			t.Fatalf("err = %v, want the plan read error", err)
		}
		if len(mem.subs) != 0 {
			t.Fatalf("%d subscriptions written, want none", len(mem.subs))
		}
	})
}
