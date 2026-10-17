package usage

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// A released row is "in an open period" only inside an active or trialing
// subscription's CURRENT period. Before the subscription started, or in an
// earlier period whose invoice is built, it is outside: Velox never re-times
// usage onto a later invoice (ADR-120). Inside the period it is invoiced
// only when a plan on the subscription prices its meter; otherwise it is
// not_on_plan, and no invoice charges it.
func TestClassifyByOpenPeriod(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	plans := map[string]domain.Plan{
		"pln_tokens": {ID: "pln_tokens", MeterIDs: []string{"mtr_tokens"}},
		"pln_seats":  {ID: "pln_seats", MeterIDs: []string{"mtr_seats"}},
	}
	active := domain.Subscription{
		Status: domain.SubscriptionActive, CurrentBillingPeriodStart: &start, CurrentBillingPeriodEnd: &end,
		Items: []domain.SubscriptionItem{{PlanID: "pln_tokens"}},
	}
	seatsOnly := active
	seatsOnly.Items = []domain.SubscriptionItem{{PlanID: "pln_seats"}}
	canceled := active
	canceled.Status = domain.SubscriptionCanceled

	at := func(ts time.Time) ReleasedUsage { return ReleasedUsage{At: ts, MeterID: "mtr_tokens"} }
	rows := []ReleasedUsage{
		at(start.Add(-time.Hour)),     // before the subscription: outside
		at(start),                     // period start is inside [start, end)
		at(end.Add(-time.Nanosecond)), // last instant inside
		at(end),                       // end is exclusive: outside
	}
	cases := []struct {
		name                  string
		subs                  []domain.Subscription
		wantIn, wantNot, wOut int
	}{
		{"active, plan prices the meter", []domain.Subscription{active}, 2, 0, 2},
		// Review finding (#1151): a plan without the meter used to count
		// these as "will be invoiced".
		{"active, plan does not price the meter", []domain.Subscription{seatsOnly}, 0, 2, 2},
		{"two subscriptions, one prices it", []domain.Subscription{seatsOnly, active}, 2, 0, 2},
		{"no subscription", nil, 0, 0, 4},
		{"canceled only", []domain.Subscription{canceled}, 0, 0, 4},
		{"no period", []domain.Subscription{{Status: domain.SubscriptionActive, Items: active.Items}}, 0, 0, 4},
	}
	for _, tc := range cases {
		in, not, out := ClassifyByOpenPeriod(rows, tc.subs, plans)
		if in != tc.wantIn || not != tc.wantNot || out != tc.wOut {
			t.Errorf("%s: in=%d not_on_plan=%d out=%d, want %d/%d/%d", tc.name, in, not, out, tc.wantIn, tc.wantNot, tc.wOut)
		}
	}
	// A plan the caller could not supply prices nothing.
	if in, not, _ := ClassifyByOpenPeriod(rows, []domain.Subscription{active}, nil); in != 0 || not != 2 {
		t.Errorf("no plans: in=%d not_on_plan=%d, want 0/2", in, not)
	}
}

// heldMemStore is a memStore that also holds rows, for the checks Hold
// makes before it reaches a store.
type heldMemStore struct {
	*memStore
	held []domain.HeldUsageEvent
}

func (h *heldMemStore) Hold(_ context.Context, _ string, ev domain.HeldUsageEvent) (domain.HeldUsageEvent, error) {
	h.held = append(h.held, ev)
	return ev, nil
}
func (h *heldMemStore) ListHeldSummary(context.Context, string) (HeldSummary, error) {
	return HeldSummary{}, nil
}
func (h *heldMemStore) HeldFor(context.Context, string, string) ([]string, int, error) {
	return nil, 0, nil
}
func (h *heldMemStore) ReleaseHeld(context.Context, string, ReleaseHeldInput, func(*sql.Tx, ReleaseHeldResult) error) (ReleaseHeldResult, error) {
	return ReleaseHeldResult{}, nil
}
func (h *heldMemStore) DiscardHeld(context.Context, string, string, string, func(*sql.Tx, int) error) (int, error) {
	return 0, nil
}
func (h *heldMemStore) IngestResolvingHeld(ctx context.Context, tenantID string, ev domain.UsageEvent) (domain.UsageEvent, error) {
	return h.Ingest(ctx, tenantID, ev)
}

// Hold refuses what a release could not ingest: no key, bad dimensions, a
// quantity the column cannot hold, a time in the future. Without a held
// store wired it reports unavailable (a 503 at the LiteLLM door), never a
// silent success.
func TestHold_Validation(t *testing.T) {
	ctx := context.Background()
	at := time.Now().UTC()
	future := at.Add(time.Hour)
	base := HoldInput{
		ExternalCustomerID: "cus_x", MeterKey: "tokens", Quantity: decimal.NewFromInt(1),
		IdempotencyKey: "k", EventAt: &at, Reason: domain.HeldReasonCustomerNotFound,
	}
	if _, err := NewService(newMemStore()).Hold(ctx, "t", base); !errors.Is(err, ErrHoldUnavailable) {
		t.Fatalf("no held store: err %v, want ErrHoldUnavailable", err)
	}

	store := &heldMemStore{memStore: newMemStore()}
	svc := NewService(store)
	bad := map[string]func(in *HoldInput){
		"no key":         func(in *HoldInput) { in.IdempotencyKey = " " },
		"nested dim":     func(in *HoldInput) { in.Dimensions = map[string]any{"a": map[string]any{}} },
		"huge quantity":  func(in *HoldInput) { in.Quantity = decimal.New(1, 27) },
		"future":         func(in *HoldInput) { in.EventAt = &future },
		"unknown reason": func(in *HoldInput) { in.Reason = "other" },
	}
	for name, mut := range bad {
		in := base
		mut(&in)
		if _, err := svc.Hold(ctx, "t", in); !errors.Is(err, errs.ErrValidation) {
			t.Errorf("%s: err %v, want a validation error", name, err)
		}
	}
	if len(store.held) != 0 {
		t.Fatalf("invalid rows reached the store: %+v", store.held)
	}

	// No time on the payload: the row is stamped with when Velox got it.
	in := base
	in.EventAt = nil
	if _, err := svc.Hold(ctx, "t", in); err != nil {
		t.Fatalf("hold: %v", err)
	}
	if got := store.held[0].EventAt; got.Before(at) || got.After(time.Now().UTC()) {
		t.Fatalf("event_at = %v, want the receive time", got)
	}
}
