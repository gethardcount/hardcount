package usage

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// A released row is "in an open period" only inside an active or trialing
// subscription's CURRENT period. Before the subscription started it is
// outside; in an earlier period whose invoice is built it is outside too
// (after_close), whatever its plan prices now: Velox never re-times usage
// onto a later invoice (ADR-120). Inside the
// period it is invoiced only when a price of the plan takes it; it is
// no_price when the plan prices its meter but no price takes its labels, and
// not_on_plan when the plan does not price its meter. A row at or after the
// period end, released before that period's invoice (the grace), belongs to
// the next period when the subscription continues; it is outside when the
// subscription ends at the period end. A sum row before a paid threshold
// invoice's end is outside; before an unpaid one's, pending.
func TestCountReleased(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	fire := start.AddDate(0, 0, 19)
	plans := map[string]PriceSet{
		"pln_tokens":   everyEvent("mtr_tokens"),
		"pln_seats":    everyEvent("mtr_seats"),
		"pln_labelled": {ByMeter: map[string][]map[string]string{"mtr_tokens": {{"model": "a"}}}},
	}
	started := start.AddDate(0, -1, 0)
	active := domain.Subscription{
		Status: domain.SubscriptionActive, StartedAt: &started, CurrentBillingPeriodStart: &start, CurrentBillingPeriodEnd: &end,
		Items: []domain.SubscriptionItem{{PlanID: "pln_tokens"}},
	}
	seatsOnly := active
	seatsOnly.Items = []domain.SubscriptionItem{{PlanID: "pln_seats"}}
	labelled := active
	labelled.Items = []domain.SubscriptionItem{{PlanID: "pln_labelled"}}
	canceled := active
	canceled.Status = domain.SubscriptionCanceled
	ending := active
	ending.CancelAtPeriodEnd = true

	at := func(ts time.Time) ReleasedUsage { return ReleasedUsage{At: ts, MeterID: "mtr_tokens"} }
	rows := []ReleasedUsage{
		at(start.Add(-time.Hour)),     // the invoiced period before: outside
		at(start),                     // period start is inside [start, end)
		at(fire.Add(-time.Hour)),      // before a threshold fire's end
		at(end.Add(-time.Nanosecond)), // last instant inside
		at(end),                       // end is exclusive: the next period's
	}
	sum := map[string]MeterBuckets{"mtr_tokens": {Additive: true}}
	type sub struct {
		s  domain.Subscription
		wm ThresholdWatermark
	}
	cases := []struct {
		name string
		subs []sub
		rows []ReleasedUsage
		want ReleaseResult
	}{
		{"active, plan prices the meter", []sub{{s: active}}, rows, ReleaseResult{InOpenPeriod: 4, OutsideOpenPeriod: 1}},
		// Review finding (#1151): a plan without the meter used to count
		// these as "will be invoiced".
		{"active, plan does not price the meter", []sub{{s: seatsOnly}}, rows, ReleaseResult{NotOnPlan: 4, OutsideOpenPeriod: 1}},
		{"two subscriptions, one prices it", []sub{{s: seatsOnly}, {s: active}}, rows, ReleaseResult{InOpenPeriod: 4, OutsideOpenPeriod: 1}},
		{"active, cancels at the period end", []sub{{s: ending}}, rows, ReleaseResult{InOpenPeriod: 3, OutsideOpenPeriod: 2}},
		// Review finding 3 on #1350: the release ignored the watermark.
		{"threshold invoice unpaid", []sub{{active, ThresholdWatermark{BilledThrough: &fire}}}, rows, ReleaseResult{InOpenPeriod: 2, PendingThresholdInvoice: 2, OutsideOpenPeriod: 1}},
		{"threshold invoice paid", []sub{{active, ThresholdWatermark{BilledThrough: &fire, PaidThrough: &fire}}}, rows, ReleaseResult{InOpenPeriod: 2, OutsideOpenPeriod: 3}},
		// Design G9: released rows no price takes were in_open_period.
		{"no price takes the rows", []sub{{s: labelled}}, rows, ReleaseResult{NoPrice: 4, OutsideOpenPeriod: 1}},
		{"a price takes the rows", []sub{{s: labelled}}, func() []ReleasedUsage {
			out := slices.Clone(rows)
			for i := range out {
				out[i].Labels = map[string]any{"model": "a"}
			}
			return out
		}(), ReleaseResult{InOpenPeriod: 4, OutsideOpenPeriod: 1}},
		{"no subscription", nil, rows, ReleaseResult{OutsideOpenPeriod: 5}},
		{"canceled only", []sub{{s: canceled}}, rows, ReleaseResult{OutsideOpenPeriod: 5}},
		{"no period", []sub{{s: domain.Subscription{Status: domain.SubscriptionActive, Items: active.Items}}}, rows, ReleaseResult{OutsideOpenPeriod: 5}},
	}
	for _, tc := range cases {
		var periods []OpenPeriod
		for _, s := range tc.subs {
			var stretches []Stretch
			windows := map[string][]Window{}
			for _, it := range s.s.Items {
				stretches = append(stretches, Stretch{PlanID: it.PlanID, From: start, To: end})
				for m := range plans[it.PlanID].ByMeter {
					windows[m] = append(windows[m], Window{PlanID: it.PlanID, From: start, To: end})
				}
			}
			if p, ok := OpenPeriodOf(s.s, stretches, windows, plans, plans, s.wm); ok {
				periods = append(periods, p)
			}
		}
		var got ReleaseResult
		countReleased(&got, tc.rows, periods, sum)
		if got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
	// A plan the caller could not supply prices nothing.
	p, _ := OpenPeriodOf(active, []Stretch{{PlanID: "pln_tokens", From: start, To: end}}, nil, nil, nil, ThresholdWatermark{})
	var got ReleaseResult
	countReleased(&got, rows, []OpenPeriod{p}, sum)
	if got != (ReleaseResult{NotOnPlan: 4, OutsideOpenPeriod: 1}) {
		t.Errorf("no plans: %+v, want not_on_plan 4 and the invoiced row outside", got)
	}
}

// heldMemStore is a memStore that also holds rows, for the checks Hold
// makes before it reaches a store.
type heldMemStore struct {
	*memStore
	held []domain.HeldUsageEvent
	// callLegacy makes IngestCallResolvingHeld report that it wrote the
	// call's pre-split mapping.
	callLegacy bool
	// callWritten, when set, is the indices IngestCallResolvingHeld reports
	// it wrote (the rest deduplicated); nil means every row of the mapping.
	callWritten []int
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
func (h *heldMemStore) IngestResolvingHeld(ctx context.Context, tenantID string, ev domain.UsageEvent, after func(*sql.Tx, domain.UsageEvent) error) (domain.UsageEvent, error) {
	return h.IngestAudited(ctx, tenantID, ev, after)
}
func (h *heldMemStore) IngestCallResolvingHeld(_ context.Context, _ string, call StoreCall, after func(*sql.Tx, CallResult) error) (CallResult, error) {
	evs := call.Events
	if h.callLegacy {
		evs = call.Legacy
	}
	written := h.callWritten
	if written == nil {
		for i := range evs {
			written = append(written, i)
		}
	}
	res := CallResult{Inserted: len(written), Deduplicated: len(evs) - len(written), Legacy: h.callLegacy, Written: written}
	if after != nil {
		if err := after(nil, res); err != nil {
			return CallResult{}, err
		}
	}
	return res, nil
}
func (h *heldMemStore) HoldCall(_ context.Context, _ string, call HeldCall) (CallResult, error) {
	h.held = append(h.held, call.Events...)
	return CallResult{Inserted: len(call.Events)}, nil
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
		ExternalCustomerID: "cus_x", EventName: "tokens", Quantity: decimal.NewFromInt(1),
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
