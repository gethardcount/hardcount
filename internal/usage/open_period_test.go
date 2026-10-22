package usage

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

var (
	opApr = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	opMay = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	opJun = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
)

func opAt(t time.Time) *time.Time { return &t }

func meters(ids ...string) map[string]bool {
	m := map[string]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// TestClassify is the rule, one case per branch.
func TestClassify(t *testing.T) {
	sum := MeterBuckets{Additive: true}
	max := MeterBuckets{}
	lastEver := MeterBuckets{LastEver: true}
	// May is the open period; April was invoiced. Started Mar 1.
	may := OpenPeriod{Start: opMay, End: opJun, Floor: opAt(opApr.AddDate(0, -1, 0)), Meters: meters("m"), NextMeters: meters("m")}
	withTrial := may
	withTrial.Floor = opAt(opApr.AddDate(0, 0, 14))
	watermark := may
	watermark.BilledThrough = opAt(opMay.AddDate(0, 0, 19))
	ending := may
	ending.NextMeters = nil
	pendingDrop := may
	pendingDrop.NextMeters = meters()
	noFloor := may
	noFloor.Floor = nil
	// Another subscription whose open period (a year) still holds April.
	yearly := OpenPeriod{Start: opApr.AddDate(0, -1, 0), End: opApr.AddDate(1, -1, 0), Floor: opAt(opApr.AddDate(0, -1, 0)), Meters: meters("other"), NextMeters: meters("other")}

	cases := []struct {
		name    string
		at      time.Time
		meter   string
		periods []OpenPeriod
		mb      MeterBuckets
		want    Class
	}{
		{"open period", opMay.AddDate(0, 0, 3), "m", []OpenPeriod{may}, sum, InOpen},
		{"open period, meter not priced", opMay.AddDate(0, 0, 3), "x", []OpenPeriod{may}, sum, NotOnPlan},
		{"invoiced period", opApr.AddDate(0, 0, 20), "m", []OpenPeriod{may}, sum, AfterClose},
		{"invoiced period, one instant before the start", opMay.Add(-time.Nanosecond), "m", []OpenPeriod{may}, sum, AfterClose},
		{"the start itself is open", opMay, "m", []OpenPeriod{may}, sum, InOpen},
		{"invoiced period, meter not priced", opApr.AddDate(0, 0, 20), "x", []OpenPeriod{may}, sum, Outside},
		{"before the subscription started", opApr.AddDate(0, -2, 0), "m", []OpenPeriod{may}, sum, Outside},
		{"in the trial", opApr.AddDate(0, 0, 3), "m", []OpenPeriod{withTrial}, sum, Outside},
		{"after the trial, invoiced", opApr.AddDate(0, 0, 20), "m", []OpenPeriod{withTrial}, sum, AfterClose},
		{"no floor known", opApr.AddDate(0, 0, 20), "m", []OpenPeriod{noFloor}, sum, Outside},
		{"last_ever meter", opApr.AddDate(0, 0, 20), "m", []OpenPeriod{may}, lastEver, Outside},
		{"max meter, invoiced period", opApr.AddDate(0, 0, 20), "m", []OpenPeriod{may}, max, AfterClose},
		{"before the watermark, sum", opMay.AddDate(0, 0, 5), "m", []OpenPeriod{watermark}, sum, AfterClose},
		{"before the watermark, max (deferred)", opMay.AddDate(0, 0, 5), "m", []OpenPeriod{watermark}, max, InOpen},
		{"before the watermark, last_ever", opMay.AddDate(0, 0, 5), "m", []OpenPeriod{watermark}, MeterBuckets{Additive: true, LastEver: true}, InOpen},
		{"after the watermark", opMay.AddDate(0, 0, 25), "m", []OpenPeriod{watermark}, sum, InOpen},
		{"grace: next period", opJun.Add(10 * time.Minute), "m", []OpenPeriod{may}, sum, InOpen},
		{"grace: ends at the period end", opJun.Add(10 * time.Minute), "m", []OpenPeriod{ending}, sum, Outside},
		{"grace: next plan drops the meter", opJun.Add(10 * time.Minute), "m", []OpenPeriod{pendingDrop}, sum, NotOnPlan},
		{"another subscription's open period holds it", opApr.AddDate(0, 0, 20), "m", []OpenPeriod{may, yearly}, sum, NotOnPlan},
		{"no subscription", opApr.AddDate(0, 0, 20), "m", nil, sum, Outside},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := classify(c.at, c.meter, c.periods, func() (MeterBuckets, error) { return c.mb, nil })
			if err != nil || got != c.want {
				t.Errorf("classify = %v, %v; want %v", got, err, c.want)
			}
		})
	}
}

// TestClassify_ReadsTheMeterOnlyWhenNeeded: an event in an open period
// never costs the meter read.
func TestClassify_ReadsTheMeterOnlyWhenNeeded(t *testing.T) {
	may := OpenPeriod{Start: opMay, End: opJun, Floor: opAt(opApr), Meters: meters("m"), NextMeters: meters("m")}
	read := 0
	if _, err := classify(opMay.AddDate(0, 0, 2), "m", []OpenPeriod{may}, func() (MeterBuckets, error) {
		read++
		return MeterBuckets{}, nil
	}); err != nil || read != 0 {
		t.Errorf("open-period event: meter read %d times, err %v; want 0", read, err)
	}
}

// TestOpenPeriodOf builds the period from a subscription: floor, the plan
// from the period end on, and whether it continues.
func TestOpenPeriodOf(t *testing.T) {
	plans := map[string]domain.Plan{"pA": {MeterIDs: []string{"m"}}, "pB": {MeterIDs: []string{"n"}}}
	base := domain.Subscription{
		Status: domain.SubscriptionActive, StartedAt: opAt(opApr), CurrentBillingPeriodStart: opAt(opMay), CurrentBillingPeriodEnd: opAt(opJun),
		Items: []domain.SubscriptionItem{{PlanID: "pA"}},
	}
	p, ok := OpenPeriodOf(base, plans, nil)
	if !ok || !p.Meters["m"] || !p.NextMeters["m"] || p.Floor == nil || !p.Floor.Equal(opApr) {
		t.Fatalf("plain: %+v %v", p, ok)
	}

	pending := base
	pending.Items = []domain.SubscriptionItem{{PlanID: "pA", PendingPlanID: "pB", PendingPlanEffectiveAt: opAt(opJun)}}
	if p, _ := OpenPeriodOf(pending, plans, nil); !p.Meters["m"] || p.NextMeters["m"] || !p.NextMeters["n"] {
		t.Errorf("pending change at the end: meters %v next %v, want m now and n next", p.Meters, p.NextMeters)
	}
	later := base
	later.Items = []domain.SubscriptionItem{{PlanID: "pA", PendingPlanID: "pB", PendingPlanEffectiveAt: opAt(opJun.AddDate(0, 1, 0))}}
	if p, _ := OpenPeriodOf(later, plans, nil); !p.NextMeters["m"] || p.NextMeters["n"] {
		t.Errorf("pending change after the end: next %v, want m", p.NextMeters)
	}

	atEnd := base
	atEnd.CancelAtPeriodEnd = true
	if p, _ := OpenPeriodOf(atEnd, plans, nil); p.NextMeters != nil {
		t.Errorf("cancel at period end: next %v, want nil", p.NextMeters)
	}
	cancelAt := base
	cancelAt.CancelAt = opAt(opJun)
	if p, _ := OpenPeriodOf(cancelAt, plans, nil); p.NextMeters != nil {
		t.Errorf("cancel_at at the end: next %v, want nil", p.NextMeters)
	}

	trial := base
	trial.TrialEndAt = opAt(opApr.AddDate(0, 0, 14))
	if p, _ := OpenPeriodOf(trial, plans, nil); p.Floor == nil || !p.Floor.Equal(*trial.TrialEndAt) {
		t.Errorf("trial: floor %v, want the trial end", p.Floor)
	}

	canceled := base
	canceled.Status = domain.SubscriptionCanceled
	if _, ok := OpenPeriodOf(canceled, plans, nil); ok {
		t.Errorf("a canceled subscription has no open period")
	}
}

// TestBucketsOf: additive only when the meter and every rule are sum/count.
func TestBucketsOf(t *testing.T) {
	rule := func(m domain.AggregationMode) domain.MeterPricingRule {
		return domain.MeterPricingRule{AggregationMode: m}
	}
	cases := []struct {
		name  string
		agg   string
		rules []domain.MeterPricingRule
		want  MeterBuckets
	}{
		{"sum meter", "sum", nil, MeterBuckets{Additive: true}},
		{"count meter", "count", nil, MeterBuckets{Additive: true}},
		{"max meter", "max", nil, MeterBuckets{}},
		{"last meter", "last", nil, MeterBuckets{}},
		{"sum meter, max rule", "sum", []domain.MeterPricingRule{rule(domain.AggSum), rule(domain.AggMax)}, MeterBuckets{}},
		{"sum meter, last_ever rule", "sum", []domain.MeterPricingRule{rule(domain.AggLastEver)}, MeterBuckets{LastEver: true}},
		{"sum meter, sum and count rules", "sum", []domain.MeterPricingRule{rule(domain.AggSum), rule(domain.AggCount)}, MeterBuckets{Additive: true}},
	}
	for _, c := range cases {
		if got := BucketsOf(domain.Meter{Aggregation: c.agg}, c.rules); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

// stepLog records reads and writes in order.
type stepLog struct{ steps []string }

// orderReader answers every event as after-close, logging its reads.
type orderReader struct {
	log *stepLog
	err error
}

func (r *orderReader) OpenPeriods(context.Context, string, string) ([]OpenPeriod, error) {
	r.log.steps = append(r.log.steps, "read")
	if r.err != nil {
		return nil, r.err
	}
	return []OpenPeriod{{Start: opMay, End: opJun, Floor: opAt(opApr), Meters: meters("mtr_1"), NextMeters: meters("mtr_1")}}, nil
}

func (r *orderReader) MeterBuckets(context.Context, string, string) (MeterBuckets, error) {
	return MeterBuckets{Additive: true}, nil
}

// orderStore logs its writes.
type orderStore struct {
	*memStore
	log *stepLog
}

func (s orderStore) IngestAudited(ctx context.Context, tenantID string, e domain.UsageEvent, emit func(tx *sql.Tx, out domain.UsageEvent) error) (domain.UsageEvent, error) {
	s.log.steps = append(s.log.steps, "write")
	return s.memStore.IngestAudited(ctx, tenantID, e, emit)
}

func (s orderStore) IngestBatch(ctx context.Context, tenantID string, events []domain.UsageEvent) ([]int, int, error) {
	s.log.steps = append(s.log.steps, "write")
	return s.memStore.IngestBatch(ctx, tenantID, events)
}

// TestIngest_ReadsOpenPeriodsBeforeTheWrite: the order is what keeps the
// flag true (open_period.go). Read after the write, a close that billed the
// row and committed between them would make the row look unbilled.
//
// Mutation check: move the classify call after the store write → the order
// is [write read].
func TestIngest_ReadsOpenPeriodsBeforeTheWrite(t *testing.T) {
	log := &stepLog{}
	svc := NewService(orderStore{newMemStore(), log})
	svc.SetOpenPeriodReader(&orderReader{log: log})
	ts := opApr.AddDate(0, 0, 20)
	ev, err := svc.Ingest(context.Background(), "t1", IngestInput{CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1), IdempotencyKey: "k1", Timestamp: &ts})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if strings.Join(log.steps, " ") != "read write" {
		t.Errorf("single ingest order = %v, want [read write]", log.steps)
	}
	if ev.BillingStatus != BillingStatusAfterClose {
		t.Errorf("billing_status = %q, want after_close", ev.BillingStatus)
	}

	log.steps = nil
	if _, errs := svc.BatchIngest(context.Background(), "t1", []IngestInput{
		{CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1), IdempotencyKey: "k2", Timestamp: &ts},
	}); len(errs) != 0 {
		t.Fatalf("batch: %v", errs)
	}
	if strings.Join(log.steps, " ") != "read write" {
		t.Errorf("batch order = %v, want [read write]", log.steps)
	}
}

// TestIngest_AfterCloseCountsWrittenRowsOnly: the counter moves for the row
// written, not for its replay, which also carries no billing_status.
func TestIngest_AfterCloseCountsWrittenRowsOnly(t *testing.T) {
	log := &stepLog{}
	svc := NewService(newMemStore())
	svc.SetOpenPeriodReader(&orderReader{log: log})
	ts := opApr.AddDate(0, 0, 20)
	in := IngestInput{CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1), IdempotencyKey: "k-count", Timestamp: &ts}
	c := afterCloseEvents.WithLabelValues(string(domain.UsageOriginAPI))
	before := testutil.ToFloat64(c)
	if ev, err := svc.Ingest(context.Background(), "t1", in); err != nil || ev.BillingStatus != BillingStatusAfterClose {
		t.Fatalf("ingest: %+v %v", ev, err)
	}
	ev, err := svc.Ingest(context.Background(), "t1", in)
	if !errors.Is(err, errs.ErrDuplicateKey) || ev.BillingStatus != "" {
		t.Fatalf("replay: %+v %v, want ErrDuplicateKey and no status", ev, err)
	}
	if got := testutil.ToFloat64(c) - before; got != 1 {
		t.Errorf("counter moved by %v, want 1", got)
	}
}

// TestIngest_OpenPeriodReadFailureIs503: a read the store could not answer
// refuses the event at every door, with nothing written, like a failed
// customer or meter lookup.
//
// Mutation check: ignore the read error and ingest → 201/202 and a row.
func TestIngest_OpenPeriodReadFailureIs503(t *testing.T) {
	const event = `{"external_customer_id":"acme","event_name":"api_call","quantity":1,"idempotency_key":"evt_periods","timestamp":"2026-04-20T00:00:00Z"}`
	doors := map[string]func(h *Handler) (func(http.ResponseWriter, *http.Request), string){
		"single": func(h *Handler) (func(http.ResponseWriter, *http.Request), string) { return h.ingest, event },
		"batch": func(h *Handler) (func(http.ResponseWriter, *http.Request), string) {
			return h.batchIngest, "[" + event + "]"
		},
		"backfill": func(h *Handler) (func(http.ResponseWriter, *http.Request), string) { return h.Backfill, event },
	}
	for door, pick := range doors {
		t.Run(door, func(t *testing.T) {
			store := newMemStore()
			svc := NewService(store)
			svc.SetOpenPeriodReader(&orderReader{log: &stepLog{}, err: errLookupInfra})
			fn, body := pick(NewHandler(svc, &stubCustomers{}, &stubMeters{}))
			rr := usageReq(t, fn, body)
			if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), `"code":"ingest_unavailable"`) {
				t.Fatalf("status %d body %s, want 503 ingest_unavailable", rr.Code, rr.Body.String())
			}
			if strings.Contains(rr.Body.String(), "connection refused") {
				t.Errorf("driver error leaked: %s", rr.Body.String())
			}
			if len(store.events) != 0 {
				t.Errorf("%d rows written, want 0", len(store.events))
			}
		})
	}
}
