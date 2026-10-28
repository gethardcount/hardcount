package usage_test

// An ingest's after_close signals follow the aggregation its event is
// counted under (ADR-130 §2, ADR-120), on real Postgres. Ingest reads the
// open periods before it writes, and the meter's aggregation on its own
// transaction after the insert. The insert's foreign key holds the meter row
// FOR KEY SHARE until commit, and an aggregation PATCH locks the meter FOR
// UPDATE before it reads whether the meter has events. So an aggregation
// PATCH that commits after the period read and before the insert is the
// aggregation the event is counted under, and the flag follows it.

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// patchBeforeTheInsert is the concurrent resolver for an ingest's
// after_close signals. Its open-period read, made before the ingest writes,
// starts an aggregation PATCH and holds it open after the PATCH has locked
// the meter and read no usage. The PATCH commits 300 ms later: after the
// period read, and before the insert can take the meter row. A meter read
// made before the insert sees the old aggregation.
type patchBeforeTheInsert struct {
	f         ingestAggFixture
	to        string
	period    usage.OpenPeriod
	paused    pausedAudit
	start     sync.Once
	commit    sync.Once
	patchErr  error
	patchDone chan struct{}
}

func (r *patchBeforeTheInsert) OpenPeriods(context.Context, string, string) ([]usage.OpenPeriod, error) {
	r.start.Do(func() {
		go func() {
			r.patchErr = r.f.patch(r.to)
			close(r.patchDone)
		}()
		select {
		case <-r.paused.inTx:
		case <-r.patchDone:
		case <-time.After(10 * time.Second):
		}
		time.AfterFunc(300*time.Millisecond, r.letCommit)
	})
	return []usage.OpenPeriod{r.period}, nil
}

func (r *patchBeforeTheInsert) letCommit() { r.commit.Do(func() { close(r.paused.release) }) }

func (r *patchBeforeTheInsert) MeterBucketsTx(ctx context.Context, tx *sql.Tx, meterID string) (usage.MeterBuckets, error) {
	m, err := pricing.NewPostgresStore(r.f.db).GetMeterTx(ctx, tx, meterID)
	if err != nil {
		return usage.MeterBuckets{}, err
	}
	return usage.BucketsOf(m), nil
}

// ingestAggFixture is a held fixture with a pricing service on the same
// database, a customer, and one meter with aggregation from and no events.
type ingestAggFixture struct {
	heldFixture
	psvc       *pricing.Service
	meter      domain.Meter
	eventName  string
	ext        string
	customerID string
}

func newIngestAggFixture(t *testing.T, name, eventName, from string) ingestAggFixture {
	t.Helper()
	f := ingestAggFixture{heldFixture: newHeldFixture(t, name), eventName: eventName, ext: "ext_" + eventName}
	f.psvc = pricing.NewService(pricing.NewPostgresStore(f.db))
	m, err := f.psvc.CreateMeter(f.ctx, f.tenantID, pricing.CreateMeterInput{
		EventName: eventName, Name: eventName, Unit: "tokens", Aggregation: from,
	})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}
	f.meter = m
	f.customerID = insertTestCustomer(t, f.db, f.tenantID, f.ext)
	return f
}

func (f ingestAggFixture) patch(agg string) error {
	_, err := f.psvc.UpdateMeter(f.ctx, f.tenantID, f.meter.ID, pricing.UpdateMeterInput{Aggregation: &agg})
	return err
}

func (f ingestAggFixture) hold(t *testing.T, key string, at time.Time) {
	t.Helper()
	if _, err := f.svc.HoldCall(f.ctx, f.tenantID, usage.HoldCallInput{
		Source: domain.HeldUsageSourceLiteLLM, ExternalCustomerID: f.ext, EventName: f.eventName,
		Reason: domain.HeldReasonCustomerNotFound,
		Events: []usage.CallEvent{{Quantity: decimal.NewFromInt(5), IdempotencyKey: key, Timestamp: &at}},
	}); err != nil {
		t.Fatalf("hold %s: %v", key, err)
	}
}

// afterCloseTotal reads hardcount_usage_unbilled_total{reason="after_close", origin}.
func afterCloseTotal(t *testing.T, origin domain.UsageEventOrigin) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "hardcount_usage_unbilled_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["reason"] == usage.BillingStatusAfterClose && labels["origin"] == string(origin) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// logCapture collects the default logger's output while a test runs.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

// afterCloseWarns counts the "billed on no invoice" WARN lines for meterID.
func (c *logCapture) afterCloseWarns(meterID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, line := range strings.Split(c.buf.String(), "\n") {
		if strings.Contains(line, "billed on no invoice") && strings.Contains(line, `"meter_id":"`+meterID+`"`) {
			n++
		}
	}
	return n
}

func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	c := &logCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(c, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return c
}

// ingestDoor runs one ingest door for the fixture's customer and meter at
// at. It returns the rows written, the rows the answer calls after_close,
// and the keys it wrote.
type ingestDoor struct {
	name   string
	origin domain.UsageEventOrigin
	run    func(t *testing.T, f ingestAggFixture, at time.Time) (written, flagged int, keys []string)
}

func ingestDoors() []ingestDoor {
	in := func(f ingestAggFixture, key string, at time.Time) usage.IngestInput {
		return usage.IngestInput{CustomerID: f.customerID, MeterID: f.meter.ID, Quantity: decimal.NewFromInt(5), IdempotencyKey: key, Timestamp: &at}
	}
	flag := func(ev domain.UsageEvent) int {
		if ev.BillingStatus == usage.BillingStatusAfterClose {
			return 1
		}
		return 0
	}
	return []ingestDoor{
		{"single", domain.UsageOriginAPI, func(t *testing.T, f ingestAggFixture, at time.Time) (int, int, []string) {
			ev, err := f.svc.Ingest(f.ctx, f.tenantID, in(f, "single-"+f.eventName, at))
			if err != nil {
				t.Fatalf("ingest: %v", err)
			}
			return 1, flag(ev), []string{"single-" + f.eventName}
		}},
		{"batch", domain.UsageOriginAPI, func(t *testing.T, f ingestAggFixture, at time.Time) (int, int, []string) {
			keys := []string{"batch-a-" + f.eventName, "batch-b-" + f.eventName}
			res, errs := f.svc.BatchIngest(f.ctx, f.tenantID, []usage.IngestInput{in(f, keys[0], at), in(f, keys[1], at)})
			if len(errs) != 0 {
				t.Fatalf("batch: %v", errs)
			}
			return res.Inserted, res.Unbilled.AfterClose, keys
		}},
		{"backfill", domain.UsageOriginBackfill, func(t *testing.T, f ingestAggFixture, at time.Time) (int, int, []string) {
			ev, err := f.svc.Backfill(f.ctx, f.tenantID, in(f, "backfill-"+f.eventName, at))
			if err != nil {
				t.Fatalf("backfill: %v", err)
			}
			return 1, flag(ev), []string{"backfill-" + f.eventName}
		}},
		{"litellm call, resend of a held key", domain.UsageOriginAPI, func(t *testing.T, f ingestAggFixture, at time.Time) (int, int, []string) {
			keys := []string{"call-" + f.eventName + ":input", "call-" + f.eventName + ":output"}
			f.hold(t, keys[0], at)
			res, err := f.svc.IngestCall(f.ctx, f.tenantID, usage.CallInput{
				CustomerID: f.customerID, MeterID: f.meter.ID,
				Events: []usage.CallEvent{
					{Quantity: decimal.NewFromInt(5), IdempotencyKey: keys[0], Timestamp: &at},
					{Quantity: decimal.NewFromInt(7), IdempotencyKey: keys[1], Timestamp: &at},
				},
			})
			if err != nil {
				t.Fatalf("ingest call: %v", err)
			}
			return res.Inserted, res.Unbilled.AfterClose, keys
		}},
	}
}

// At every ingest door, an aggregation PATCH that commits after the
// ingest's open-period read and before its insert decides the after_close
// signals: the answer's billing_status (or after_close count),
// hardcount_usage_unbilled_total{reason="after_close"} and the "billed on no invoice" WARN. The
// event is before a paid threshold invoice's end: under sum the close does
// not count it (after_close), under max it does (not flagged).
//
// Mutation-verify: read the meter's aggregation before the insert again →
// the PATCH commits between that read and the insert, and the signals
// follow the old aggregation. Drop the usage_events → meters foreign key →
// the insert no longer waits for the PATCH, the read on the ingest's
// transaction sees the old aggregation, and the same.
func TestIngest_AfterCloseFollowsTheAggregationTheEventIsCountedUnder(t *testing.T) {
	for _, door := range ingestDoors() {
		for _, tc := range []struct{ from, to string }{{"sum", "max"}, {"max", "sum"}} {
			t.Run(door.name+": "+tc.from+" to "+tc.to, func(t *testing.T) {
				eventName := "tokens_" + strings.NewReplacer(" ", "_", ",", "").Replace(door.name) + "_" + tc.from
				f := newIngestAggFixture(t, "Ingest agg "+door.name+" "+tc.from, eventName, tc.from)
				logs := captureLogs(t)

				now := time.Now().UTC().Truncate(time.Second)
				at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
				catchAll := usage.PriceSet{ByMeter: map[string][]map[string]string{f.meter.ID: {{}}}}
				r := &patchBeforeTheInsert{f: f, to: tc.to, paused: newPausedAudit(), patchDone: make(chan struct{}), period: usage.OpenPeriod{
					Start: now.Add(-2 * time.Hour), End: now.Add(24 * time.Hour), Floor: at(-3 * time.Hour),
					Plans: map[string]usage.PriceSet{"pln": catchAll}, Stretches: []usage.Stretch{{PlanID: "pln", From: now.Add(-2 * time.Hour), To: now.Add(24 * time.Hour)}},
					Windows:            map[string][]usage.Window{f.meter.ID: {{PlanID: "pln", From: now.Add(-2 * time.Hour), To: now.Add(24 * time.Hour)}}},
					ThresholdWatermark: usage.ThresholdWatermark{BilledThrough: at(-30 * time.Minute), PaidThrough: at(-30 * time.Minute)},
				}}
				f.psvc.SetAuditLogger(r.paused)
				f.svc.SetOpenPeriodReader(r)

				before := afterCloseTotal(t, door.origin)
				written, flagged, keys := door.run(t, f, *at(-time.Hour))
				select {
				case <-r.patchDone:
				case <-time.After(10 * time.Second):
					t.Fatal("the PATCH never finished")
				}
				if r.patchErr != nil {
					t.Fatalf("PATCH %s to %s before the insert: %v", tc.from, tc.to, r.patchErr)
				}

				m, err := f.psvc.GetMeter(f.ctx, f.tenantID, f.meter.ID)
				if err != nil {
					t.Fatalf("get meter: %v", err)
				}
				if m.Aggregation != tc.to {
					t.Fatalf("aggregation = %q, want %s", m.Aggregation, tc.to)
				}
				if n := f.countEvents(t, keys...); n != written || written != len(keys) {
					t.Fatalf("written %d, usage events %d, want %d", written, n, len(keys))
				}
				want := 0
				if usage.BucketsOf(m).Additive {
					want = written
				}
				if flagged != want {
					t.Errorf("after_close in the answer = %d, want %d: the events are counted under %s", flagged, want, m.Aggregation)
				}
				if got := afterCloseTotal(t, door.origin) - before; got != float64(want) {
					t.Errorf("hardcount_usage_unbilled_total{reason=\"after_close\",origin=%q} moved by %v, want %d", door.origin, got, want)
				}
				if got := logs.afterCloseWarns(f.meter.ID); got != want {
					t.Errorf("after_close WARN lines = %d, want %d", got, want)
				}

				f.psvc.SetAuditLogger(nil)
				wantMeterHasUsage(t, "PATCH after the ingest", f.patch(tc.from))
			})
		}
	}
}
