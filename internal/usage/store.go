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
}

// Aggregate is the response shape of GET /v1/usage-events/aggregate. It
// powers the stat cards + "Usage by Meter" breakdown on the dashboard's
// /usage page so they reflect server-side filtered totals rather than
// reductions over the current page of events.
//
// TotalUnits is decimal-string-encoded (NUMERIC(38,12) per ADR-005) so
// fractional GPU-hours and partial tokens round-trip without loss.
type Aggregate struct {
	TotalEvents     int             `json:"total_events"`
	TotalUnits      decimal.Decimal `json:"total_units"`
	ActiveMeters    int             `json:"active_meters"`
	ActiveCustomers int             `json:"active_customers"`
	ByMeter         []MeterTotal    `json:"by_meter"`
}

// MeterTotal is the per-meter row in Aggregate.ByMeter — one entry per
// distinct meter_id matching the filter, sorted by Total DESC so the
// dashboard's horizontal-bar breakdown can render in priority order.
type MeterTotal struct {
	MeterID string          `json:"meter_id"`
	Total   decimal.Decimal `json:"total"`
}
