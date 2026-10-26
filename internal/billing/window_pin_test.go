package billing

import (
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// Each stretch of a period is pinned where its item joined the stretch's
// plan, or at the period start when it joined before (ADR-130 §4 step 3,
// amended 2026-10-26). A quantity change does not move the pin; a swap does.
// The item threshold reads an item's current plan at its latest stretch's
// pin, and from the item's own fields when that stretch is not in the read.
//
// Mutation: pin every stretch at the period start, or restart the run on a
// quantity change, and the pins below move.
func TestPlanStretches_PinEachStretchWhereItsItemJoined(t *testing.T) {
	ps := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	pe := ps.AddDate(0, 1, 0)
	started := ps.Add(15 * time.Hour)
	qty := ps.AddDate(0, 0, 3)
	swap := ps.AddDate(0, 0, 9)
	sub := domain.Subscription{
		CreatedAt: started, StartedAt: &started,
		Items: []domain.SubscriptionItem{{ID: "it", PlanID: "pln_b", CreatedAt: started, PlanChangedAt: &swap}},
	}
	rows := map[string][]domain.ItemInterval{"it": {
		{SubscriptionItemID: "it", PlanID: "pln_a", Quantity: 1, StartsAt: ps, EndsAt: &qty, ItemCreatedAt: started},
		{SubscriptionItemID: "it", PlanID: "pln_a", Quantity: 2, StartsAt: qty, EndsAt: &swap, ItemCreatedAt: started},
		{SubscriptionItemID: "it", PlanID: "pln_b", Quantity: 2, StartsAt: swap, ItemCreatedAt: started},
	}}
	stretches := planStretches(sub, intervalSegmentsByItem(rows, ps, pe), ps, pe, ps)
	want := []struct {
		plan  string
		start time.Time
		pin   time.Time
	}{{"pln_a", ps, started}, {"pln_a", qty, started}, {"pln_b", swap, swap}}
	if len(stretches) != len(want) {
		t.Fatalf("stretches = %+v, want %d", stretches, len(want))
	}
	for i, w := range want {
		s := stretches[i]
		if s.planID != w.plan || !s.start.Equal(w.start) || !s.pin.at.Equal(w.pin) {
			t.Errorf("stretch %d = %s from %v pinned %v, want %s from %v pinned %v", i, s.planID, s.start, s.pin.at, w.plan, w.start, w.pin)
		}
	}
	if got := itemPin(sub, stretches, sub.Items[0], ps); !got.at.Equal(swap) {
		t.Errorf("item pin = %v, want the swap %v", got.at, swap)
	}
	// A read that ends before the swap has no stretch on plan B: the pin
	// comes from the item's last swap.
	early := planStretches(sub, intervalSegmentsByItem(rows, ps, qty), ps, qty, ps)
	if got := itemPin(sub, early, sub.Items[0], ps); !got.at.Equal(swap) {
		t.Errorf("item pin without a plan B stretch = %v, want the swap %v", got.at, swap)
	}

	// The next period: every stretch joined before it, so all are pinned at
	// its start.
	next := pe.AddDate(0, 1, 0)
	for _, s := range planStretches(sub, intervalSegmentsByItem(rows, pe, next), pe, next, pe) {
		if !s.pin.at.Equal(pe) {
			t.Errorf("next period: %s pinned %v, want the period start %v", s.planID, s.pin.at, pe)
		}
	}
}
