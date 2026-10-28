package usage

import (
	"context"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
)

// TestIngest_LateEventCounted pins the late-event visibility contract
// (2026-07-10 design review): a live event stamped >24h in the past is
// ACCEPTED (late events are industry-standard; a hard reject breaks retries)
// but counted on hardcount_usage_late_event_total + WARNed, because it may fall
// inside an already-finalized period where it is stored-but-unbilled. A
// fresh event is not counted; backfill-origin events are excluded (the
// documented-safe intentional path).
func TestIngest_LateEventCounted(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()

	apiCount := func() float64 {
		return promtestutil.ToFloat64(lateUsageEvents.WithLabelValues("api"))
	}
	backfillCount := func() float64 {
		return promtestutil.ToFloat64(lateUsageEvents.WithLabelValues("backfill"))
	}

	baseAPI, baseBackfill := apiCount(), backfillCount()

	// Fresh event: no count.
	if _, err := svc.Ingest(ctx, "t1", IngestInput{
		CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1),
	}); err != nil {
		t.Fatalf("fresh ingest: %v", err)
	}
	if got := apiCount() - baseAPI; got != 0 {
		t.Errorf("fresh event must not count as late, got +%v", got)
	}

	// >24h-late live event: accepted + counted.
	late := time.Now().UTC().Add(-25 * time.Hour)
	if _, err := svc.Ingest(ctx, "t1", IngestInput{
		CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1), Timestamp: &late,
	}); err != nil {
		t.Fatalf("late ingest must still be accepted: %v", err)
	}
	if got := apiCount() - baseAPI; got != 1 {
		t.Errorf("late live event must increment the counter once, got +%v", got)
	}

	// Backfill at the same lateness: excluded.
	if _, err := svc.Backfill(ctx, "t1", IngestInput{
		CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1), Timestamp: &late,
	}); err != nil {
		t.Fatalf("backfill ingest: %v", err)
	}
	if got := backfillCount() - baseBackfill; got != 0 {
		t.Errorf("backfill events are deliberate operator posts and must not count, got +%v", got)
	}
}

// A LiteLLM call with cache writes arrives as two mappings, of which the
// store writes one. Only the rows written count as late: a replay of the
// probe call (four split rows, two pre-split rows) more than 24h late moves
// the counter by 4 when the split is written and by 2 when the pre-split
// mapping is, never by 6.
// Mutation-verify: record lateness while building both mappings → +6.
func TestIngestCall_LateCountsOnlyTheWrittenMapping(t *testing.T) {
	ctx := context.Background()
	late := time.Now().UTC().Add(-48 * time.Hour)
	ev := func(key string, q int64) CallEvent {
		return CallEvent{Quantity: dec(q), IdempotencyKey: key, Timestamp: &late}
	}
	in := CallInput{
		CustomerID: "cus_1", MeterID: "mtr_1",
		Events:    []CallEvent{ev("c:input", 1000), ev("c:cache_write_5m", 40000), ev("c:cache_write_1h", 60000), ev("c:output", 500)},
		Legacy:    []CallEvent{ev("c:input", 101000), ev("c:output", 500)},
		LegacyKey: "c:input",
	}
	count := func() float64 { return promtestutil.ToFloat64(lateUsageEvents.WithLabelValues("api")) }

	for _, tc := range []struct {
		legacy bool
		want   float64
	}{{false, 4}, {true, 2}} {
		store := &heldMemStore{memStore: newMemStore(), callLegacy: tc.legacy}
		before := count()
		if _, err := NewService(store).IngestCall(ctx, "t1", in); err != nil {
			t.Fatalf("legacy=%v: ingest call: %v", tc.legacy, err)
		}
		if got := count() - before; got != tc.want {
			t.Errorf("legacy=%v: late counter moved by %v, want %v (the rows written)", tc.legacy, got, tc.want)
		}
	}
}
