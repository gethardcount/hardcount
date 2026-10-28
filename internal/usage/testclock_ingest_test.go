package usage

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// fakeResolver implements clock.Resolver the way the wired
// billing.Engine does: frozen_time for pinned customers, wall-clock
// for everyone else. calls counts lookups so tests can assert the
// live-mode path never pays the resolver query.
type fakeResolver struct {
	frozen map[string]time.Time // customerID → frozen_time
	calls  int
}

func (f *fakeResolver) SimForCustomer(_ context.Context, _, customerID string) (clock.Sim, error) {
	f.calls++
	if t, ok := f.frozen[customerID]; ok {
		return clock.Sim{At: t, TestClockID: "tc_stub"}, nil
	}
	return clock.Sim{At: time.Now().UTC()}, nil
}

func (f *fakeResolver) SimForSubscription(_ context.Context, _, _ string) (clock.Sim, error) {
	return clock.Sim{At: time.Now().UTC()}, nil
}

func (f *fakeResolver) SimForInvoice(_ context.Context, _, _ string) (clock.Sim, error) {
	return clock.Sim{At: time.Now().UTC()}, nil
}

// TestIngest_TestClockPinnedCustomer locks the Tier A ingest fix from the
// test-clock design audit: every ingest path gated event timestamps
// against WALL-clock time.Now(), so on a clock advanced into the future
// (the flagship usage demo: advance a month, ingest, see it billed)
// every simulated-time event was rejected as "in the future" and
// no-timestamp events landed at wall-clock — before the simulated
// period, where the next Advance never bills them.
func TestIngest_TestClockPinnedCustomer(t *testing.T) {
	frozen := time.Now().UTC().Add(45 * 24 * time.Hour) // clock advanced ~6 weeks ahead
	resolver := &fakeResolver{frozen: map[string]time.Time{"cus_pinned": frozen}}
	svc := NewService(newMemStore())
	svc.SetResolver(resolver)
	ctx := postgres.WithLivemode(context.Background(), false) // test mode

	t.Run("no timestamp lands at frozen_time, not wall-clock", func(t *testing.T) {
		e, err := svc.Ingest(ctx, "t1", IngestInput{
			CustomerID: "cus_pinned", MeterID: "mtr_1", Quantity: dec(5),
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !e.Timestamp.Equal(frozen) {
			t.Errorf("timestamp: got %v, want frozen_time %v", e.Timestamp, frozen)
		}
	})

	t.Run("simulated-time timestamp (wall-clock future) is accepted", func(t *testing.T) {
		simTS := frozen.Add(-1 * time.Hour) // "an hour ago" in simulated time
		e, err := svc.Ingest(ctx, "t1", IngestInput{
			CustomerID: "cus_pinned", MeterID: "mtr_1", Quantity: dec(1), Timestamp: &simTS,
		})
		if err != nil {
			t.Fatalf("sim-time event on an advanced clock must be accepted (pre-fix: rejected as future): %v", err)
		}
		if !e.Timestamp.Equal(simTS) {
			t.Errorf("timestamp: got %v, want %v", e.Timestamp, simTS)
		}
	})

	t.Run("beyond frozen_time + skew is rejected", func(t *testing.T) {
		beyond := frozen.Add(1 * time.Hour)
		_, err := svc.Ingest(ctx, "t1", IngestInput{
			CustomerID: "cus_pinned", MeterID: "mtr_1", Quantity: dec(1), Timestamp: &beyond,
		})
		if err == nil {
			t.Error("event past the clock's frozen_time must be rejected")
		}
	})

	t.Run("unpinned customer keeps wall-clock gating", func(t *testing.T) {
		future := time.Now().UTC().Add(2 * time.Hour)
		_, err := svc.Ingest(ctx, "t1", IngestInput{
			CustomerID: "cus_unpinned", MeterID: "mtr_1", Quantity: dec(1), Timestamp: &future,
		})
		if err == nil {
			t.Error("wall-clock-future event on an unpinned customer must still be rejected")
		}
		before := time.Now().UTC()
		e, err := svc.Ingest(ctx, "t1", IngestInput{
			CustomerID: "cus_unpinned", MeterID: "mtr_1", Quantity: dec(1),
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if e.Timestamp.Before(before.Add(-time.Minute)) || e.Timestamp.After(time.Now().UTC().Add(time.Minute)) {
			t.Errorf("unpinned default timestamp must be ~wall-clock now, got %v", e.Timestamp)
		}
	})
}

// TestIngest_LivemodeSkipsResolver pins the hot-path contract: test
// clocks are test-mode-only (DB CHECK), so livemode ingest must never
// pay the per-event customer lookup.
func TestIngest_LivemodeSkipsResolver(t *testing.T) {
	resolver := &fakeResolver{frozen: map[string]time.Time{"cus_pinned": time.Now().UTC().Add(24 * time.Hour)}}
	svc := NewService(newMemStore())
	svc.SetResolver(resolver)
	ctx := postgres.WithLivemode(context.Background(), true)

	if _, err := svc.Ingest(ctx, "t1", IngestInput{
		CustomerID: "cus_pinned", MeterID: "mtr_1", Quantity: dec(1),
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolver.calls != 0 {
		t.Errorf("livemode ingest must not consult the clock resolver, got %d calls", resolver.calls)
	}
}

// TestBackfill_TestClockPinnedCustomer: the backfill past-only gate
// compares against the customer's effective now, so simulated history
// can be backfilled onto an advanced clock even when the timestamps are
// in the wall-clock future.
func TestBackfill_TestClockPinnedCustomer(t *testing.T) {
	frozen := time.Now().UTC().Add(45 * 24 * time.Hour)
	resolver := &fakeResolver{frozen: map[string]time.Time{"cus_pinned": frozen}}
	svc := NewService(newMemStore())
	svc.SetResolver(resolver)
	ctx := postgres.WithLivemode(context.Background(), false)

	t.Run("simulated past (wall-clock future) accepted", func(t *testing.T) {
		simPast := time.Now().UTC().Add(30 * 24 * time.Hour) // before frozen, after wall now
		e, err := svc.Backfill(ctx, "t1", IngestInput{
			CustomerID: "cus_pinned", MeterID: "mtr_1", Quantity: dec(3), Timestamp: &simPast,
		})
		if err != nil {
			t.Fatalf("sim-past backfill on an advanced clock must be accepted: %v", err)
		}
		if e.Origin != domain.UsageOriginBackfill {
			t.Errorf("origin: got %q, want backfill", e.Origin)
		}
		if resolverCallsAfterChain := resolver.calls; resolverCallsAfterChain != 1 {
			t.Errorf("Backfill→ingest chain must resolve once (ctx carries it), got %d calls", resolverCallsAfterChain)
		}
	})

	t.Run("beyond frozen_time rejected", func(t *testing.T) {
		beyond := frozen.Add(1 * time.Hour)
		_, err := svc.Backfill(ctx, "t1", IngestInput{
			CustomerID: "cus_pinned", MeterID: "mtr_1", Quantity: dec(1), Timestamp: &beyond,
		})
		if err == nil {
			t.Error("backfill past the clock's frozen_time must be rejected")
		}
	})
}

// TestIngest_CtxBoundEffectiveNowWins: an upstream entry point that
// already bound effective-now (clock.WithEffectiveNow) short-circuits
// the resolver — binding at the boundary, inheritance below it.
func TestIngest_CtxBoundEffectiveNowWins(t *testing.T) {
	resolver := &fakeResolver{}
	svc := NewService(newMemStore())
	svc.SetResolver(resolver)
	bound := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	ctx := clock.WithEffectiveNow(postgres.WithLivemode(context.Background(), false), bound)

	e, err := svc.Ingest(ctx, "t1", IngestInput{
		CustomerID: "cus_any", MeterID: "mtr_1", Quantity: dec(1),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !e.Timestamp.Equal(bound) {
		t.Errorf("timestamp: got %v, want ctx-bound %v", e.Timestamp, bound)
	}
	if resolver.calls != 0 {
		t.Errorf("ctx-bound effective-now must skip the resolver, got %d calls", resolver.calls)
	}
}
