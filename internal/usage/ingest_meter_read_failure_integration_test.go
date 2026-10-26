package usage_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/usage"
)

var errMeterRead = errors.New("meter read failed")

// failingMeterReader answers one open period whose class depends on the
// meter's aggregation, and fails every meter read.
type failingMeterReader struct{ period usage.OpenPeriod }

func (r failingMeterReader) OpenPeriods(context.Context, string, string) ([]usage.OpenPeriod, error) {
	return []usage.OpenPeriod{r.period}, nil
}

func (failingMeterReader) MeterBucketsTx(context.Context, *sql.Tx, string) (usage.MeterBuckets, error) {
	return usage.MeterBuckets{}, errMeterRead
}

// A meter read that fails on the write's transaction, after the insert,
// rolls the write back at every door that reads it there. The ingest doors
// return errIngestUnavailable (503 ingest_unavailable) and write nothing;
// the release returns the error and its rows stay held. Without this, a
// failed read would commit rows with no class: an event the close will not
// bill would get no after_close flag, counter or WARN.
//
// Mutation-verify: in any one store method (IngestAudited, IngestBatch,
// IngestResolvingHeld, IngestCallResolvingHeld, ReleaseHeld), ignore the
// hook's error → that door commits its rows and its case fails.
func TestIngest_FailedMeterReadWritesNothing(t *testing.T) {
	in := func(f ingestAggFixture, key string, at time.Time) usage.IngestInput {
		return usage.IngestInput{CustomerID: f.customerID, MeterID: f.meter.ID, Quantity: decimal.NewFromInt(5), IdempotencyKey: key, Timestamp: &at}
	}
	doors := []struct {
		name string
		// release: the door is the held-usage release, whose error is not
		// errIngestUnavailable.
		release bool
		// run returns the keys it tried to write and the held keys among
		// them, which must stay held.
		run func(t *testing.T, f ingestAggFixture, at time.Time) (keys, held []string, err error)
	}{
		{"single", false, func(t *testing.T, f ingestAggFixture, at time.Time) ([]string, []string, error) {
			_, err := f.svc.Ingest(f.ctx, f.tenantID, in(f, "single-fail", at))
			return []string{"single-fail"}, nil, err
		}},
		{"batch", false, func(t *testing.T, f ingestAggFixture, at time.Time) ([]string, []string, error) {
			keys := []string{"batch-fail-a", "batch-fail-b"}
			_, errs := f.svc.BatchIngest(f.ctx, f.tenantID, []usage.IngestInput{in(f, keys[0], at), in(f, keys[1], at)})
			if len(errs) != 1 {
				t.Fatalf("batch errors = %v, want one", errs)
			}
			return keys, nil, errs[0]
		}},
		{"backfill", false, func(t *testing.T, f ingestAggFixture, at time.Time) ([]string, []string, error) {
			_, err := f.svc.Backfill(f.ctx, f.tenantID, in(f, "backfill-fail", at))
			return []string{"backfill-fail"}, nil, err
		}},
		{"litellm event, resend of a held key", false, func(t *testing.T, f ingestAggFixture, at time.Time) ([]string, []string, error) {
			key := "held-fail:input"
			f.hold(t, key, at)
			_, err := f.svc.IngestResolvingHeld(f.ctx, f.tenantID, in(f, key, at))
			return []string{key}, []string{key}, err
		}},
		{"litellm call, resend of a held key", false, func(t *testing.T, f ingestAggFixture, at time.Time) ([]string, []string, error) {
			keys := []string{"call-fail:input", "call-fail:output"}
			f.hold(t, keys[0], at)
			_, err := f.svc.IngestCall(f.ctx, f.tenantID, usage.CallInput{
				CustomerID: f.customerID, MeterID: f.meter.ID,
				Events: []usage.CallEvent{
					{Quantity: decimal.NewFromInt(5), IdempotencyKey: keys[0], Timestamp: &at},
					{Quantity: decimal.NewFromInt(7), IdempotencyKey: keys[1], Timestamp: &at},
				},
			})
			return keys, keys[:1], err
		}},
		{"held-usage release", true, func(t *testing.T, f ingestAggFixture, at time.Time) ([]string, []string, error) {
			keys := []string{"release-fail:input", "release-fail:output"}
			f.hold(t, keys[0], at)
			f.hold(t, keys[1], at)
			_, err := f.svc.Release(f.ctx, f.tenantID, usage.ReleaseInput{
				ExternalCustomerID: f.ext, CustomerID: f.customerID,
				MeterIDs: map[string]string{f.eventName: f.meter.ID},
			})
			return keys, keys, err
		}},
	}
	for i, door := range doors {
		t.Run(door.name, func(t *testing.T) {
			eventName := "tokens_meter_read_fail_" + string(rune('a'+i))
			f := newIngestAggFixture(t, "Ingest meter read fails "+door.name, eventName, "sum")

			now := time.Now().UTC().Truncate(time.Second)
			at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
			catchAll := usage.PriceSet{ByMeter: map[string][]map[string]string{f.meter.ID: {{}}}}
			// The event is before a paid threshold invoice's end, so its
			// class depends on the meter's aggregation and the meter is read.
			f.svc.SetOpenPeriodReader(failingMeterReader{period: usage.OpenPeriod{
				Start: now.Add(-2 * time.Hour), End: now.Add(24 * time.Hour), Floor: at(-3 * time.Hour),
				Next:  []usage.PriceSet{catchAll},
				Plans: map[string]usage.PriceSet{"pln": catchAll}, Stretches: []usage.Stretch{{PlanID: "pln", From: now.Add(-2 * time.Hour), To: now.Add(24 * time.Hour)}},
				Windows:            map[string][]usage.Window{f.meter.ID: {{PlanID: "pln", From: now.Add(-2 * time.Hour), To: now.Add(24 * time.Hour)}}},
				ThresholdWatermark: usage.ThresholdWatermark{BilledThrough: at(-30 * time.Minute), PaidThrough: at(-30 * time.Minute)},
			}})

			keys, held, err := door.run(t, f, *at(-time.Hour))
			if !errors.Is(err, errMeterRead) {
				t.Fatalf("err = %v, want the failed meter read", err)
			}
			if !door.release && !errors.Is(err, usage.ErrIngestUnavailable) {
				t.Errorf("err = %v, want errIngestUnavailable (503 ingest_unavailable)", err)
			}
			if n := f.countEvents(t, keys...); n != 0 {
				t.Errorf("usage events written = %d, want 0", n)
			}
			for _, k := range held {
				if status, _ := f.heldStatus(t, k); status != string(domain.HeldStatusHeld) {
					t.Errorf("held row %s = %s, want held", k, status)
				}
			}
		})
	}
}
