package usage

import (
	"context"
	"database/sql"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
)

type Store interface {
	Ingest(ctx context.Context, tenantID string, event domain.UsageEvent) (domain.UsageEvent, error)
	// IngestAudited runs the caller-supplied audit emission on the ingest's
	// own transaction (ADR-090). Only BACKFILL passes an emitter — live
	// machine ingest passes nil (see PostgresStore.IngestAudited).
	IngestAudited(ctx context.Context, tenantID string, event domain.UsageEvent, emit func(tx *sql.Tx, out domain.UsageEvent) error) (domain.UsageEvent, error)
	// IngestBatch writes every event in ONE transaction — all-or-nothing,
	// so a client retry after a mid-batch abort never re-ingests a
	// committed prefix. Idempotency replays are counted in deduped, not
	// errors. Returns (inserted, deduped, err).
	IngestBatch(ctx context.Context, tenantID string, events []domain.UsageEvent) (int, int, error)
	// GetByIdempotencyKey returns the event a replayed key originally
	// wrote (errs.ErrNotFound when absent). Backs replay-as-success on
	// the public ingest door.
	GetByIdempotencyKey(ctx context.Context, tenantID, key string) (domain.UsageEvent, error)
	List(ctx context.Context, filter ListFilter) ([]domain.UsageEvent, int, error)
	Aggregate(ctx context.Context, filter ListFilter) (Aggregate, error)
	AggregateForBillingPeriod(ctx context.Context, tenantID, customerID string, meterIDs []string, from, to time.Time) (map[string]decimal.Decimal, error)
	AggregateForBillingPeriodByAgg(ctx context.Context, tenantID, customerID string, meters map[string]string, from, to time.Time) (map[string]decimal.Decimal, error)

	// AggregateByPricingRules walks meter_pricing_rules in priority-DESC
	// order and claims each in-period event to its top-priority matching
	// rule (JSONB superset on properties), then aggregates per rule using
	// the rule's aggregation_mode. Events that match no rule are returned
	// as an unclaimed entry (RuleID=="") aggregated with defaultMode.
	//
	// last_ever rules ignore the period start and pick the latest event
	// stamped before `to` (for "current state" billing like seat counts):
	// the value as of the window end, however late the window is read.
	AggregateByPricingRules(ctx context.Context, tenantID, customerID, meterID string, defaultMode domain.AggregationMode, from, to time.Time) ([]domain.RuleAggregation, error)

	// AggregateDailyBuckets returns events bucketed to UTC-day granularity
	// over [from, to) for the given customer × meter set. One row per
	// (bucket_start, meter_id) — the service fills missing buckets with
	// zero so chart consumers get continuous time. Sums to the same
	// per-meter total as AggregateForBillingPeriod over the same window;
	// powers the daily-bar-chart on the customer-usage view (matches
	// Datadog / OpenAI / AWS Cost Explorer's primary visual primitive).
	AggregateDailyBuckets(ctx context.Context, tenantID, customerID string, meterIDs []string, from, to time.Time) ([]DailyBucketRow, error)
}

// DailyBucketRow is one (bucket_start, meter) cell from the bucket
// aggregation. Storage-shape only; the service composes these into
// the gap-filled DailyBucket presentation type before serving.
type DailyBucketRow struct {
	BucketStart time.Time
	MeterID     string
	Quantity    decimal.Decimal
}

type ListFilter struct {
	TenantID   string
	CustomerID string
	MeterID    string
	From       *time.Time
	To         *time.Time
	// Dimensions filters events whose properties JSONB CONTAINS every
	// pair (`properties @> $1` — multi-key containment is AND; served by
	// the GIN index from migration 0062). Values are typed: the handler
	// parses "cached=true" / "attempt=2" as JSON literals and everything
	// else as strings, matching how ingest stored them. Pre-2026-07-05
	// the dashboard SENT this param and the server silently dropped it —
	// unfiltered data rendered as filtered.
	Dimensions map[string]any
	Limit      int
	// Offset-based pagination (legacy path). Mutually exclusive with
	// AfterTimestamp+AfterID — the cursor path takes precedence when
	// both are provided.
	Offset int
	// Cursor-based pagination (2026-05-29). Seek-method query:
	// WHERE (timestamp, id) < (AfterTimestamp, AfterID). Both must
	// be set together; the handler rejects partial cursor sets.
	// Stable across concurrent inserts at the table's head — offset
	// pagination page-skewed reliably on usage_events whenever a
	// cycle close fired between operator pages (per-API-call write
	// volume = highest of any Velox table).
	AfterTimestamp time.Time
	AfterID        string
	// DayLoc is read by Aggregate only: the per-meter daily series buckets
	// events by calendar day in this location. nil means UTC.
	DayLoc *time.Location
}

// Aggregate is the response shape of GET /v1/usage-events/aggregate. It
// powers the stat cards + "Usage by Meter" breakdown on the dashboard's
// /usage page so they reflect server-side filtered totals rather than
// reductions over the current page of events.
//
// TotalUnits is decimal-string-encoded (NUMERIC(38,12) per ADR-005) so
// fractional GPU-hours and partial tokens round-trip without loss. It adds
// quantities across meters, whatever their units and modes, so it is kept
// for API compatibility only; the dashboard shows ByMeter[].Value instead.
type Aggregate struct {
	TotalEvents     int             `json:"total_events"`
	TotalUnits      decimal.Decimal `json:"total_units"`
	ActiveMeters    int             `json:"active_meters"`
	ActiveCustomers int             `json:"active_customers"`
	ByMeter         []MeterTotal    `json:"by_meter"`
}

// MeterTotal is the per-meter row in Aggregate.ByMeter — one entry per
// distinct meter_id matching the filter, sorted by Total DESC.
//
// Total is the plain sum of reported quantities. Value is the meter's
// usage under its own aggregation (Service.Aggregate fills it from the
// meter's mode), because a sum is the wrong number for a Maximum or a
// Latest-value meter: three Storage readings of 140, 210 and 120 GB are a
// 210 GB peak, not 470 GB.
//
//   - sum:   Value = sum of quantities
//   - count: Value = number of events
//   - max:   Value = each customer's highest reading in the window, added
//     across customers
//   - last:  Value = each customer's latest reading in the window, added
//     across customers
//
// Billing applies max and last per billing period; the window here is the
// caller's filter, which the dashboard labels.
type MeterTotal struct {
	MeterID string          `json:"meter_id"`
	Total   decimal.Decimal `json:"total"`
	// Aggregation is the meter's mode used for Value ("" when unknown;
	// Value then equals Total).
	Aggregation string          `json:"aggregation"`
	Value       decimal.Decimal `json:"value"`
	Events      int             `json:"events"`
	Customers   int             `json:"customers"`
	// Peak is the highest single reported quantity.
	Peak  decimal.Decimal `json:"peak"`
	Daily []DailyValue    `json:"daily"`

	// Candidates the store computes; the service picks one per mode.
	sumOfCustomerMax  decimal.Decimal
	sumOfCustomerLast decimal.Decimal
}

// DailyValue is one calendar day of a meter's usage under the meter's
// mode (sum, count, per-customer peak or latest reading, added across
// customers). Days without events are absent.
type DailyValue struct {
	Date  string          `json:"date"` // YYYY-MM-DD in the request's tz
	Value decimal.Decimal `json:"value"`

	sum, count, sumOfCustomerMax, sumOfCustomerLast decimal.Decimal
}

// AggregationMixed is the by_meter aggregation of a meter whose events
// bill under more than one mode. Its value is the plain sum of reported
// quantities, which billing may not use.
const AggregationMixed = "mixed"

// BillingModes works out, for each meter, the one aggregation billing uses
// for its events: "sum", "count", "max", "last", or AggregationMixed.
//
// Billing reads the mode from each pricing rule (meters.aggregation became
// advisory in migration 0054). Events no rule claims bill under the
// meter's own aggregation, so that mode counts too unless a rule with an
// empty dimension_match claims everything. last_during_period and
// last_ever both read here as "last": the latest reading in the window.
//
// Billing aggregates each bucket on its own — one per rule, plus the
// unclaimed events when no rule claims everything (AggregateByPricingRules)
// — and adds the buckets up. A sum or a count adds up the same either way,
// but a peak or a latest reading does not: two rules' peaks added together
// are not the peak of all the events. So a max or last meter with more than
// one bucket is AggregationMixed too.
func BillingModes(meters []domain.Meter, rules []domain.MeterPricingRule) map[string]string {
	ruleModes := map[string]map[string]bool{}
	ruleCount := map[string]int{}
	catchAll := map[string]bool{}
	for _, r := range rules {
		if ruleModes[r.MeterID] == nil {
			ruleModes[r.MeterID] = map[string]bool{}
		}
		ruleModes[r.MeterID][ruleMode(r.AggregationMode)] = true
		ruleCount[r.MeterID]++
		if len(r.DimensionMatch) == 0 {
			catchAll[r.MeterID] = true
		}
	}
	out := make(map[string]string, len(meters))
	for _, m := range meters {
		set := map[string]bool{}
		for mode := range ruleModes[m.ID] {
			set[mode] = true
		}
		if !catchAll[m.ID] {
			set[meterMode(m.Aggregation)] = true
		}
		buckets := ruleCount[m.ID]
		if !catchAll[m.ID] {
			buckets++
		}
		out[m.ID] = AggregationMixed
		if len(set) == 1 {
			for mode := range set {
				if buckets == 1 || (mode != "max" && mode != "last") {
					out[m.ID] = mode
				}
			}
		}
	}
	return out
}

func ruleMode(m domain.AggregationMode) string {
	switch m {
	case domain.AggCount:
		return "count"
	case domain.AggMax:
		return "max"
	case domain.AggLastDuringPeriod, domain.AggLastEver:
		return "last"
	default:
		return "sum"
	}
}

// meterMode reads meters.aggregation the way billing does
// (mapMeterAggregation): anything unrecognised bills as a sum.
func meterMode(agg string) string {
	switch agg {
	case "count", "max", "last":
		return agg
	default:
		return "sum"
	}
}

// pick returns the candidate that matches an aggregation mode. An unknown
// or empty mode, or AggregationMixed, falls back to the plain sum.
func pick(mode string, sum, count, sumMax, sumLast decimal.Decimal) decimal.Decimal {
	switch mode {
	case "count":
		return count
	case "max":
		return sumMax
	case "last":
		return sumLast
	default:
		return sum
	}
}

// ApplyMeterModes fills Aggregation and Value on every row (and its daily
// series) from the meter's aggregation mode. modes maps meter id to mode.
func (a *Aggregate) ApplyMeterModes(modes map[string]string) {
	for i := range a.ByMeter {
		m := &a.ByMeter[i]
		mode := modes[m.MeterID]
		m.Aggregation = mode
		m.Value = pick(mode, m.Total, decimal.NewFromInt(int64(m.Events)), m.sumOfCustomerMax, m.sumOfCustomerLast)
		for j := range m.Daily {
			d := &m.Daily[j]
			d.Value = pick(mode, d.sum, d.count, d.sumOfCustomerMax, d.sumOfCustomerLast)
		}
	}
}
