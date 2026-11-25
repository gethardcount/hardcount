package subscription

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// downSettings fails every read, as a settings DB blip would.
type downSettings struct{}

func (downSettings) Get(_ context.Context, _ string) (domain.TenantSettings, error) {
	return domain.TenantSettings{}, errors.New("settings read: connection reset")
}

// The service stores the period boundaries and anchor day every later close
// bills from, in the tenant zone. A failed settings read must refuse the
// write, not store UTC-anchored boundaries (P24).

func TestCreate_TimezoneUnreadableFailsWithNothingWritten(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, clock.NewFake(time.Date(2026, 5, 1, 14, 0, 0, 0, time.UTC)))
	svc.SetSettingsReader(downSettings{})

	if _, err := svc.Create(context.Background(), "t1", CreateInput{
		Code: "s-tz-down", DisplayName: "n", CustomerID: "c",
		Items: []CreateItemInput{{PlanID: "p"}},
	}); err == nil {
		t.Fatal("Create succeeded with unreadable settings; want an error")
	}
	if n := len(store.subs); n != 0 {
		t.Errorf("subscriptions written: got %d, want 0", n)
	}
}

func TestCrossIntervalSwap_TimezoneUnreadableLeavesPlanUntouched(t *testing.T) {
	ctx := context.Background()
	fb := &fakeBiller{createTxOK: true}
	svc, sub := newSwapWiringSvc(t, fb)
	svc.SetSettingsReader(downSettings{})

	if _, err := svc.UpdateItemTx(ctx, nil, "t1", sub.ID, sub.Items[0].ID, UpdateItemInput{
		NewPlanID: "p_monthly_adv", Immediate: true,
	}); err == nil {
		t.Fatal("cross-interval swap succeeded with unreadable settings; want an error")
	}
	after, err := svc.store.Get(ctx, "t1", sub.ID)
	if err != nil {
		t.Fatalf("re-read sub: %v", err)
	}
	if after.Items[0].PlanID != "p_yearly_adv" {
		t.Errorf("plan changed despite the failure: got %q", after.Items[0].PlanID)
	}
	if !after.CurrentBillingPeriodStart.Equal(*sub.CurrentBillingPeriodStart) || after.BillingAnchorDay != sub.BillingAnchorDay {
		t.Errorf("period or anchor moved despite the failure")
	}
}
