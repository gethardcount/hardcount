package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

type PostgresStore struct {
	db *postgres.DB
}

func NewPostgresStore(db *postgres.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

func (s *PostgresStore) Ingest(ctx context.Context, tenantID string, event domain.UsageEvent) (domain.UsageEvent, error) {
	return s.IngestAudited(ctx, tenantID, event, nil)
}

// IngestAudited is Ingest with a hook, emit, run on the ingest's transaction
// after the insert, only when the row was written. An emit error rolls the
// write back.
//
// The single and backfill doors read the meter's aggregation there
// (open_period.go). Batch reads it in IngestBatch's hook, and the LiteLLM
// doors in IngestResolvingHeld's and IngestCallResolvingHeld's. Only
// BACKFILL also writes an audit row there (ADR-090):
// live ingest (POST /v1/usage-events, /batch, the LiteLLM spend callback) is
// machine metering at up to ~1000/s, usage_events IS the record, and a row
// per event would double the write volume of the hottest path for no
// forensic gain. An operator inserting BACKDATED usage is changing what a
// customer will be billed for a period that may already have closed — an
// operator action on the money path, not machine telemetry. Sharing the
// ingest tx means a backfilled event that cannot be recorded is not ingested
// at all.
func (s *PostgresStore) IngestAudited(ctx context.Context, tenantID string, event domain.UsageEvent, emit func(tx *sql.Tx, out domain.UsageEvent) error) (domain.UsageEvent, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.UsageEvent{}, err
	}
	defer postgres.Rollback(tx)

	event, err = ingestOneTx(ctx, tx, tenantID, event)
	if err != nil {
		return domain.UsageEvent{}, err
	}
	if emit != nil {
		if err := emit(tx, event); err != nil {
			return domain.UsageEvent{}, fmt.Errorf("ingest emission: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.UsageEvent{}, err
	}
	return event, nil
}

// IngestBatch writes every event in ONE transaction — all-or-nothing.
// Pre-fix, BatchIngest looped one tx per event: a mid-batch abort (client
// timeout, connection drop, crash) left a COMMITTED PREFIX, and the
// standard client response — retry the whole batch — re-ingested that
// prefix. Atomic batches rule out the committed prefix. They do not make a
// retry safe by themselves: a batch that committed and lost its response is
// written again unless its events carry keys, which is why the HTTP door
// requires one on every event (#980).
//
// Keyed duplicates (replay of a fully-committed batch, or the same key
// twice within one batch) are counted in deduped, not errors — matching
// the LiteLLM door's replay contract. inserted lists the indices of the
// events written, so the caller can tell which rows this batch wrote. after,
// when not nil, runs on the transaction after every insert, with inserted;
// an error from it rolls the batch back.
func (s *PostgresStore) IngestBatch(ctx context.Context, tenantID string, events []domain.UsageEvent, after func(tx *sql.Tx, inserted []int) error) (inserted []int, deduped int, err error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, 0, err
	}
	defer postgres.Rollback(tx)

	for i, event := range events {
		_, err := ingestOneTx(ctx, tx, tenantID, event)
		switch {
		case errors.Is(err, errs.ErrDuplicateKey):
			deduped++
		case err != nil:
			return nil, 0, fmt.Errorf("event[%d]: %w", i, err)
		default:
			inserted = append(inserted, i)
		}
	}
	if after != nil {
		if err := after(tx, inserted); err != nil {
			return nil, 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, err
	}
	return inserted, deduped, nil
}

// GetByIdempotencyKey fetches the event a replayed key originally wrote.
// Backs replay-as-success on the public ingest door: instead of a bare
// 409, the handler returns the original row (Stripe idempotency shape).
// Livemode scoping rides the RLS session like every other reader.
func (s *PostgresStore) GetByIdempotencyKey(ctx context.Context, tenantID, key string) (domain.UsageEvent, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.UsageEvent{}, err
	}
	defer postgres.Rollback(tx)

	var event domain.UsageEvent
	err = tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, customer_id, meter_id,
			quantity, properties, COALESCE(idempotency_key,''), timestamp, origin,
			provider_cost_micros, COALESCE(provider_cost_source,'')
		FROM usage_events WHERE idempotency_key = $1`, key,
	).Scan(&event.ID, &event.TenantID, &event.CustomerID, &event.MeterID,
		&event.Quantity, propertiesScanner{&event.Dimensions},
		&event.IdempotencyKey, &event.Timestamp, &event.Origin,
		&event.ProviderCostMicros, &event.ProviderCostSource)
	if err == sql.ErrNoRows {
		return domain.UsageEvent{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.UsageEvent{}, err
	}
	return event, nil
}

// ingestOneTx is the single-source INSERT every ingest path funnels
// through (live POST, batch, backfill, LiteLLM). Runs on the caller's tx
// so Ingest keeps its own-commit shape while IngestBatch composes N
// inserts atomically. Returns errs.ErrDuplicateKey (no row written) when
// the event's idempotency key already exists — via ON CONFLICT DO NOTHING
// rather than a unique-violation error, because a violation would poison
// the surrounding batch transaction.
func ingestOneTx(ctx context.Context, tx *sql.Tx, tenantID string, event domain.UsageEvent) (domain.UsageEvent, error) {
	id := postgres.NewID("vlx_evt")

	origin := string(event.Origin)
	if origin == "" {
		origin = string(domain.UsageOriginAPI)
	}

	props, err := propertiesJSON(event.Dimensions)
	if err != nil {
		return domain.UsageEvent{}, err
	}

	// Provider-cost stamp (ADR-079): COGS attaches at INGEST — the
	// universal verified snapshot semantic; later rate edits are
	// non-retroactive by construction. Resolution runs as a scalar
	// subquery INSIDE this INSERT (every ingest path — live POST, batch,
	// backfill, LiteLLM — funnels through here, so the site-set is closed
	// by construction, and it adds zero extra round trips on the hottest
	// write path; an empty rate table costs ~nothing via the unique
	// index). Key order per ADR-079 D3: model_raw exact first (raw ids
	// price per snapshot), then the canonical model family token. Micros
	// = cost_per_token × quantity × 1e6, ROUND HALF UP. Source semantics:
	// 'table' when a rate matched; 'not_applicable' when the event has no
	// costable dims (non-token meters — keeps the honesty counter
	// meaningful); NULL when costable dims exist but no rate matched, or
	// when the cost does not fit BIGINT micros ('unresolved' — the
	// actionable signal). A quantity and a rate inside their limits can
	// price past BIGINT; the stamp is the operator's cost, not what the
	// customer is billed, so the event is still stored and billed, and the
	// margin report counts it as unresolved.
	err = tx.QueryRowContext(ctx, `
		WITH rate AS (
			SELECT r.cost_per_token
			FROM provider_cost_rates r
			WHERE r.tenant_id = $2
			  AND r.provider = $6::jsonb->>'provider'
			  AND r.token_type = $6::jsonb->>'token_type'
			  AND r.model IN ($6::jsonb->>'model_raw', $6::jsonb->>'model')
			ORDER BY (r.model IS NOT DISTINCT FROM $6::jsonb->>'model_raw') DESC
			LIMIT 1
		), stamp AS (
			SELECT micros FROM (
				SELECT ROUND(cost_per_token * $5 * 1000000) AS micros FROM rate
			) c
			WHERE micros BETWEEN -9223372036854775808 AND 9223372036854775807
		)
		INSERT INTO usage_events (id, tenant_id, customer_id, meter_id,
			quantity, properties, idempotency_key, timestamp, origin,
			provider_cost_micros, provider_cost_source)
		SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9,
			CASE
				WHEN $10::BIGINT IS NOT NULL THEN $10::BIGINT
				WHEN (SELECT COUNT(*) FROM stamp) = 1
					THEN (SELECT micros FROM stamp)::BIGINT
			END,
			CASE
				WHEN $10::BIGINT IS NOT NULL THEN 'observed'
				WHEN $6::jsonb->>'provider' IS NULL
				  OR $6::jsonb->>'token_type' IS NULL
				  OR ($6::jsonb->>'model' IS NULL AND $6::jsonb->>'model_raw' IS NULL)
					THEN 'not_applicable'
				WHEN (SELECT COUNT(*) FROM stamp) = 1 THEN 'table'
			END
		ON CONFLICT (tenant_id, livemode, idempotency_key) DO NOTHING
		RETURNING id, tenant_id, customer_id, meter_id,
			quantity, properties, COALESCE(idempotency_key,''), timestamp, origin,
			provider_cost_micros, COALESCE(provider_cost_source,'')
	`, id, tenantID, event.CustomerID, event.MeterID,
		event.Quantity,
		props, postgres.NullableString(event.IdempotencyKey),
		event.Timestamp, origin,
		// $10 — the provider's own per-half cost when the caller supplied
		// one (ADR-079 D4). NULL keeps the rate-table path exactly as it
		// was; non-NULL short-circuits both CASEs above.
		event.ProviderCostMicros,
	).Scan(&event.ID, &event.TenantID, &event.CustomerID, &event.MeterID,
		&event.Quantity, propertiesScanner{&event.Dimensions},
		&event.IdempotencyKey, &event.Timestamp, &event.Origin,
		&event.ProviderCostMicros, &event.ProviderCostSource)

	// ON CONFLICT DO NOTHING returns no row on an idempotency replay —
	// surfaced as ErrDuplicateKey to keep this store's contract identical
	// to the pre-batch unique-violation shape (callers branch on it).
	if err == sql.ErrNoRows {
		return domain.UsageEvent{}, fmt.Errorf("%w: idempotency_key %q", errs.ErrDuplicateKey, event.IdempotencyKey)
	}
	if err != nil {
		return domain.UsageEvent{}, err
	}
	return event, nil
}

// List paginates usage events ordered by (timestamp DESC, id DESC).
// Returns events + total (for the legacy offset path; 0 when cursor
// path used). Supports two mutually-exclusive paging shapes:
//
//   - Cursor (preferred, 2026-05-29): filter.AfterTimestamp +
//     filter.AfterID set → seek-method query
//     `WHERE (timestamp, id) < (after_ts, after_id)`. Skips the
//     COUNT query, fetches limit+1 to detect hasMore via the next
//     handler call. Stable across concurrent inserts at the table's
//     head — usage_events is the highest-write table in Velox, so
//     offset-based pagination page-skewed reliably whenever a cycle
//     close fired between operator pages.
//   - Offset (legacy): COUNT + LIMIT/OFFSET. Kept for the dashboard
//     paths that still use offset+total for "Page 1 of N" UX.
//
// id as the tiebreaker keeps ordering deterministic when many events
// share a microsecond — common for batched ingestion.
func (s *PostgresStore) List(ctx context.Context, filter ListFilter) ([]domain.UsageEvent, int, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, filter.TenantID)
	if err != nil {
		return nil, 0, err
	}
	defer postgres.Rollback(tx)

	// Default 100, clamp to 1000 — usage list intentionally allows a
	// higher cap than other stores (usage volume is naturally higher;
	// clients page through it). No-silent-fallbacks principle.
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	} else if limit > 1000 {
		limit = 1000
	}

	where, args := buildUsageWhere(filter)
	useCursor := !filter.AfterTimestamp.IsZero() && filter.AfterID != ""

	if useCursor {
		args = append(args, filter.AfterTimestamp, filter.AfterID)
		clause := fmt.Sprintf(`(timestamp, id) < ($%d, $%d)`, len(args)-1, len(args))
		if where == "" {
			where = " WHERE " + clause
		} else {
			where += " AND " + clause
		}
	}

	// Cursor path: fetch limit+1, skip the COUNT — the handler
	// derives hasMore from the over-fetch. Offset path: COUNT first
	// so callers get a "Page X of N" total.
	var total int
	if !useCursor {
		countQuery := `SELECT COUNT(*) FROM usage_events` + where
		if err := tx.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
			return nil, 0, err
		}
	}

	queryLimit := limit
	if useCursor {
		queryLimit = limit + 1
	}
	args = append(args, queryLimit)
	query := `SELECT id, tenant_id, customer_id, meter_id,
		quantity, properties, COALESCE(idempotency_key,''), timestamp,
		provider_cost_micros, COALESCE(provider_cost_source,'')
		FROM usage_events` + where + ` ORDER BY timestamp DESC, id DESC LIMIT $` + fmt.Sprintf("%d", len(args))
	if !useCursor {
		args = append(args, filter.Offset)
		query += ` OFFSET $` + fmt.Sprintf("%d", len(args))
	}

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	var events []domain.UsageEvent
	for rows.Next() {
		var e domain.UsageEvent
		if err := rows.Scan(&e.ID, &e.TenantID, &e.CustomerID, &e.MeterID,
			&e.Quantity, propertiesScanner{&e.Dimensions},
			&e.IdempotencyKey, &e.Timestamp,
			&e.ProviderCostMicros, &e.ProviderCostSource); err != nil {
			return nil, 0, err
		}
		events = append(events, e)
	}
	return events, total, rows.Err()
}

// Aggregate returns the totals + per-meter breakdown that powers the
// /usage page. It honours the same filter as List (customer / meter /
// from / to / dimensions) but ignores limit/offset — the whole point is to
// surface filtered totals, not page slices.
//
// SQL strategy: one row-of-totals query, then one per-meter query and one
// per-(day, meter) query. The per-meter queries first roll up per
// customer (sum, count, peak, latest reading) so the service can pick the
// value that matches each meter's aggregation mode: a Maximum meter is the
// sum of each customer's peak, never the sum of every reading. All three
// run inside the same tenant tx so RLS scopes them identically. SUM is
// decimal-precision (NUMERIC(38,12)).
func (s *PostgresStore) Aggregate(ctx context.Context, filter ListFilter) (Aggregate, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, filter.TenantID)
	if err != nil {
		return Aggregate{}, err
	}
	defer postgres.Rollback(tx)

	where, args := buildUsageWhere(filter)

	var agg Aggregate
	err = tx.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(quantity), 0),
			COUNT(DISTINCT meter_id),
			COUNT(DISTINCT customer_id)
		FROM usage_events`+where, args...,
	).Scan(&agg.TotalEvents, &agg.TotalUnits, &agg.ActiveMeters, &agg.ActiveCustomers)
	if err != nil {
		return Aggregate{}, fmt.Errorf("aggregate totals: %w", err)
	}

	// Latest reading: id DESC breaks same-timestamp ties, the same order
	// AggregateForBillingPeriodByAgg's "last" query bills with.
	rows, err := tx.QueryContext(ctx, `
		WITH per_customer AS (
			SELECT meter_id, customer_id,
				SUM(quantity) AS s,
				COUNT(*) AS c,
				MAX(quantity) AS mx,
				(ARRAY_AGG(quantity ORDER BY timestamp DESC, id DESC))[1] AS lst
			FROM usage_events`+where+`
			GROUP BY meter_id, customer_id
		)
		SELECT meter_id, SUM(s), SUM(c)::bigint, COUNT(*), MAX(mx), SUM(mx), SUM(lst)
		FROM per_customer
		GROUP BY meter_id
		ORDER BY 2 DESC, meter_id`, args...)
	if err != nil {
		return Aggregate{}, fmt.Errorf("aggregate by_meter: %w", err)
	}
	defer func() { _ = rows.Close() }()

	index := map[string]int{}
	for rows.Next() {
		var m MeterTotal
		if err := rows.Scan(&m.MeterID, &m.Total, &m.Events, &m.Customers, &m.Peak, &m.sumOfCustomerMax, &m.sumOfCustomerLast); err != nil {
			return Aggregate{}, err
		}
		m.Value = m.Total
		m.Daily = []DailyValue{}
		index[m.MeterID] = len(agg.ByMeter)
		agg.ByMeter = append(agg.ByMeter, m)
	}
	if err := rows.Err(); err != nil {
		return Aggregate{}, err
	}
	_ = rows.Close()
	if agg.ByMeter == nil {
		agg.ByMeter = []MeterTotal{}
		return agg, nil
	}

	loc := "UTC"
	if filter.DayLoc != nil {
		loc = filter.DayLoc.String()
	}
	dayArgs := append(append([]any{}, args...), loc)
	tzParam := fmt.Sprintf("$%d", len(dayArgs))
	days, err := tx.QueryContext(ctx, `
		WITH per_customer AS (
			SELECT to_char(timestamp AT TIME ZONE `+tzParam+`, 'YYYY-MM-DD') AS d, meter_id, customer_id,
				SUM(quantity) AS s,
				COUNT(*) AS c,
				MAX(quantity) AS mx,
				(ARRAY_AGG(quantity ORDER BY timestamp DESC, id DESC))[1] AS lst
			FROM usage_events`+where+`
			GROUP BY 1, meter_id, customer_id
		)
		SELECT d, meter_id, SUM(s), SUM(c), SUM(mx), SUM(lst)
		FROM per_customer
		GROUP BY d, meter_id
		ORDER BY d`, dayArgs...)
	if err != nil {
		return Aggregate{}, fmt.Errorf("aggregate daily: %w", err)
	}
	defer func() { _ = days.Close() }()
	for days.Next() {
		var meterID string
		var d DailyValue
		if err := days.Scan(&d.Date, &meterID, &d.sum, &d.count, &d.sumOfCustomerMax, &d.sumOfCustomerLast); err != nil {
			return Aggregate{}, err
		}
		d.Value = d.sum
		if i, ok := index[meterID]; ok {
			agg.ByMeter[i].Daily = append(agg.ByMeter[i].Daily, d)
		}
	}
	if err := days.Err(); err != nil {
		return Aggregate{}, err
	}
	return agg, nil
}

func (s *PostgresStore) AggregateForBillingPeriodByAgg(ctx context.Context, tenantID, customerID string, meters map[string]string, from, to time.Time) (map[string]decimal.Decimal, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	result := make(map[string]decimal.Decimal)
	if len(meters) == 0 {
		return result, nil
	}

	// One query per DISTINCT aggregation function (at most four) rather
	// than one per meter. A subscription spanning M meters cost M round
	// trips per call — and a period close calls this once per distinct
	// usage window (the whole period, and each mid-period segment), so the
	// fan-out was multiplicative in M.
	byAgg := make(map[string][]string, 4)
	for meterID, agg := range meters {
		byAgg[agg] = append(byAgg[agg], meterID)
	}
	aggs := make([]string, 0, len(byAgg))
	for agg := range byAgg {
		aggs = append(aggs, agg)
	}
	sort.Strings(aggs)

	for _, agg := range aggs {
		meterIDs := byAgg[agg]
		sort.Strings(meterIDs)

		args := []any{tenantID, customerID, from, to}
		placeholders := make([]string, len(meterIDs))
		for i, id := range meterIDs {
			placeholders[i] = fmt.Sprintf("$%d", i+5)
			args = append(args, id)
		}
		in := strings.Join(placeholders, ",")

		// tenant_id is in the WHERE clause even though the tx is already
		// RLS-scoped. The policy is an OR (bypass_rls OR tenant match),
		// which Postgres can only apply as a post-scan Filter — never an
		// Index Cond. Every usage_events btree leads with tenant_id, so
		// without this predicate the leading column is unconstrained and
		// the aggregate seq-scans EVERY tenant's events. Same defect class
		// migration 0147 fixed for audit_log.
		var query string
		if agg == "last" {
			// Latest in-period event per meter. id DESC breaks
			// same-timestamp ties so the billed total is reproducible
			// across replicas (same rationale as ranked_rules below).
			query = `SELECT DISTINCT ON (meter_id) meter_id, COALESCE(quantity, 0)
				FROM usage_events
				WHERE tenant_id = $1 AND customer_id = $2
				  AND timestamp >= $3 AND timestamp < $4
				  AND meter_id IN (` + in + `)
				ORDER BY meter_id, timestamp DESC, id DESC`
		} else {
			aggFunc := "SUM"
			switch agg {
			case "max":
				aggFunc = "MAX"
			case "count":
				aggFunc = "COUNT"
			}
			query = fmt.Sprintf(`SELECT meter_id, COALESCE(%s(quantity), 0)
				FROM usage_events
				WHERE tenant_id = $1 AND customer_id = $2
				  AND timestamp >= $3 AND timestamp < $4
				  AND meter_id IN (%s)
				GROUP BY meter_id`, aggFunc, in)
		}

		// Propagate query errors — swallowing them silently drops the
		// meter from the billing total (under-billing at finalize).
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("aggregate meters (%s): %w", agg, err)
		}
		scanErr := func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var meterID string
				var val decimal.Decimal
				if err := rows.Scan(&meterID, &val); err != nil {
					return err
				}
				// A zero aggregate is absent from the map, the same as a
				// meter with no events in the period (no row at all).
				// A total below zero (corrections larger than the usage)
				// is returned as it is: the billing engine bills it 0
				// and records it (#1193). Dropping it here hid it.
				if !val.IsZero() {
					result[meterID] = val
				}
			}
			return rows.Err()
		}()
		if scanErr != nil {
			return nil, fmt.Errorf("aggregate meters (%s): %w", agg, scanErr)
		}
	}

	return result, nil
}

// AggregateDailyBuckets — see Store interface for contract. UTC-day
// granularity matches every reference platform (Datadog, OpenAI, AWS
// Cost Explorer); finer grain (hour) lives in a future bucket-grain
// param when an operator needs it. NULL meter_ids → empty result with
// no DB roundtrip. The result is unsorted; the service fills gaps and
// sorts by (bucket_start, meter_id) before serving.
func (s *PostgresStore) AggregateDailyBuckets(ctx context.Context, tenantID, customerID string, meterIDs []string, from, to time.Time) ([]DailyBucketRow, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	if len(meterIDs) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(meterIDs))
	args := []any{tenantID, customerID, from, to}
	for i, id := range meterIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+5)
		args = append(args, id)
	}

	// tenant_id predicate: see AggregateForBillingPeriodByAgg — RLS alone
	// cannot drive an index scan here.
	rows, err := tx.QueryContext(ctx, `
		SELECT date_trunc('day', timestamp AT TIME ZONE 'UTC') AS bucket_start,
		       meter_id,
		       COALESCE(SUM(quantity), 0) AS qty
		FROM usage_events
		WHERE tenant_id = $1
		  AND customer_id = $2
		  AND timestamp >= $3
		  AND timestamp < $4
		  AND meter_id IN (`+strings.Join(placeholders, ",")+`)
		GROUP BY 1, 2
	`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []DailyBucketRow
	for rows.Next() {
		var row DailyBucketRow
		if err := rows.Scan(&row.BucketStart, &row.MeterID, &row.Quantity); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *PostgresStore) AggregateForBillingPeriod(ctx context.Context, tenantID, customerID string, meterIDs []string, from, to time.Time) (map[string]decimal.Decimal, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	result := make(map[string]decimal.Decimal)
	if len(meterIDs) == 0 {
		return result, nil
	}

	placeholders := make([]string, len(meterIDs))
	args := []any{tenantID, customerID, from, to}
	for i, id := range meterIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+5)
		args = append(args, id)
	}

	// tenant_id predicate: see AggregateForBillingPeriodByAgg.
	rows, err := tx.QueryContext(ctx, `
		SELECT meter_id, COALESCE(SUM(quantity), 0)
		FROM usage_events
		WHERE tenant_id = $1 AND customer_id = $2
			AND timestamp >= $3 AND timestamp < $4
			AND meter_id IN (`+strings.Join(placeholders, ",")+`)
		GROUP BY meter_id
	`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var meterID string
		var total decimal.Decimal
		if err := rows.Scan(&meterID, &total); err != nil {
			return nil, err
		}
		result[meterID] = total
	}
	return result, rows.Err()
}

// AggregateClaims rolls a customer's events of one meter in [from, to) into
// one quantity per claim, under the meter's aggregation (ADR-130 §2). Each
// event belongs to the first claim in the list whose match its dimensions
// contain (JSONB containment, `properties @> match`), so no event is counted
// twice; an event no claim takes is the unclaimed bucket (RuleID ""). The
// caller lists the claims in rank order (domain.ClaimsOfRules), so the
// claim is the caller's and every caller claims the same way.
//
// The aggregation is the meter's, one of sum, count, max and last. last is
// the latest event by (timestamp DESC, id DESC), the order every read of a
// last meter uses (AggregateForBillingPeriodByAgg, Aggregate): two events
// at one instant are a tie the id breaks, so every path bills the same one.
//
// A bucket's quantity is the aggregation over that bucket's events alone, so
// a max or last meter with several claims bills each claim's own peak or
// latest value.
func (s *PostgresStore) AggregateClaims(
	ctx context.Context,
	tenantID, customerID, meterID, aggregation string,
	claims []domain.UsageClaim,
	from, to time.Time,
) ([]domain.RuleAggregation, error) {
	// A request that carries a memo (the public cost dashboard) runs each
	// distinct aggregation once; see AggregationMemo. Every other caller,
	// including every billing writer, has no memo and always reads the table.
	if memo, ok := ctx.Value(aggregationMemoKey{}).(*AggregationMemo); ok {
		return memo.through(aggregationMemoKeyFor(tenantID, customerID, meterID, aggregation, claims, from, to), func() ([]domain.RuleAggregation, error) {
			return s.aggregateClaims(ctx, tenantID, customerID, meterID, aggregation, claims, from, to)
		})
	}
	return s.aggregateClaims(ctx, tenantID, customerID, meterID, aggregation, claims, from, to)
}

// claimAggregate is the SQL aggregate of each meter aggregation over one
// bucket's events.
var claimAggregate = map[string]string{
	domain.AggregationSum:   `COALESCE(SUM(quantity), 0)`,
	domain.AggregationCount: `COUNT(*)::numeric`,
	domain.AggregationMax:   `COALESCE(MAX(quantity), 0)`,
	domain.AggregationLast:  `(array_agg(quantity ORDER BY timestamp DESC, id DESC))[1]`,
}

func (s *PostgresStore) aggregateClaims(
	ctx context.Context,
	tenantID, customerID, meterID, aggregation string,
	claims []domain.UsageClaim,
	from, to time.Time,
) ([]domain.RuleAggregation, error) {
	agg, ok := claimAggregate[aggregation]
	if !ok {
		return nil, fmt.Errorf("aggregate claims: meter %s has aggregation %q, not one of sum, count, max, last", meterID, aggregation)
	}
	if claims == nil {
		claims = []domain.UsageClaim{}
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return nil, fmt.Errorf("aggregate claims: marshal claims: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	// The claims arrive as one jsonb array; WITH ORDINALITY keeps the
	// caller's order as the claim rank. tenant_id is in the WHERE clause
	// for the index (see AggregateForBillingPeriodByAgg).
	rows, err := tx.QueryContext(ctx, `
		WITH claims AS (
			SELECT c->>'rule_id'                AS rule_id,
			       c->>'rating_rule_version_id' AS rating_rule_version_id,
			       c->'match'                   AS match,
			       ord
			FROM jsonb_array_elements($6::jsonb) WITH ORDINALITY AS t(c, ord)
		),
		claimed AS (
			SELECT e.id, e.quantity, e.timestamp,
			       c.rule_id, c.rating_rule_version_id
			FROM usage_events e
			LEFT JOIN LATERAL (
				SELECT rule_id, rating_rule_version_id
				FROM claims
				WHERE e.properties @> claims.match
				ORDER BY ord
				LIMIT 1
			) c ON TRUE
			WHERE e.tenant_id   = $1
			  AND e.meter_id    = $2
			  AND e.customer_id = $3
			  AND e.timestamp  >= $4
			  AND e.timestamp  <  $5
		)
		SELECT COALESCE(rule_id, ''), COALESCE(rating_rule_version_id, ''), `+agg+`
		FROM claimed
		GROUP BY rule_id, rating_rule_version_id
	`, tenantID, meterID, customerID, from, to, string(claimsJSON))
	if err != nil {
		return nil, fmt.Errorf("aggregate claims: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []domain.RuleAggregation
	for rows.Next() {
		var r domain.RuleAggregation
		if err := rows.Scan(&r.RuleID, &r.RatingRuleVersionID, &r.Quantity); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func buildUsageWhere(f ListFilter) (string, []any) {
	var clauses []string
	var args []any
	idx := 1

	// Leading predicate, and the reason this is not redundant with the
	// RLS-scoped tx: the policy is an OR (bypass_rls OR tenant match), so
	// Postgres applies it as a post-scan Filter and can never use it as an
	// Index Cond. Every usage_events btree leads with tenant_id — without
	// this clause the list, the COUNT, and the /usage stat-card aggregate
	// all seq-scan every tenant's events.
	if f.TenantID != "" {
		clauses = append(clauses, fmt.Sprintf("tenant_id = $%d", idx))
		args = append(args, f.TenantID)
		idx++
	}
	if f.CustomerID != "" {
		clauses = append(clauses, fmt.Sprintf("customer_id = $%d", idx))
		args = append(args, f.CustomerID)
		idx++
	}
	if f.MeterID != "" {
		clauses = append(clauses, fmt.Sprintf("meter_id = $%d", idx))
		args = append(args, f.MeterID)
		idx++
	}
	if f.From != nil {
		clauses = append(clauses, fmt.Sprintf("timestamp >= $%d", idx))
		args = append(args, *f.From)
		idx++
	}
	if f.To != nil {
		clauses = append(clauses, fmt.Sprintf("timestamp < $%d", idx))
		args = append(args, *f.To)
		idx++
	}
	if len(f.Dimensions) > 0 {
		// One containment doc carrying ALL pairs — JSONB @> on a
		// multi-key object is AND semantics, and the GIN index on
		// properties (migration 0062) serves it. Marshal cannot fail
		// for a map built from query-string scalars.
		doc, _ := json.Marshal(f.Dimensions)
		clauses = append(clauses, fmt.Sprintf("properties @> $%d::jsonb", idx))
		args = append(args, string(doc))
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// propertiesJSON serializes the event's free-form properties for the JSONB
// column. This map is also the carrier for multi-dim meter dimensions
// (model, operation, region, etc.) that pricing_rule.dimension_match runs
// subset-match against — losing it here would silently drop dimension
// information at ingest, so a marshal failure is treated as a hard error.
func propertiesJSON(props map[string]any) (string, error) {
	if len(props) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(props)
	if err != nil {
		return "", fmt.Errorf("marshal usage_event.properties: %w", err)
	}
	return string(b), nil
}

// propertiesScanner adapts a *map[string]any to sql.Scanner so SELECT
// statements can read the JSONB column straight into the struct field.
// The pgx driver hands JSONB to Scan as []byte (or string in some paths).
type propertiesScanner struct{ dst *map[string]any }

func (s propertiesScanner) Scan(src any) error {
	if src == nil {
		*s.dst = nil
		return nil
	}
	var raw []byte
	switch v := src.(type) {
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		return fmt.Errorf("unsupported scan type for properties: %T", src)
	}
	if len(raw) == 0 {
		*s.dst = nil
		return nil
	}
	m := map[string]any{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("unmarshal usage_event.properties: %w", err)
	}
	*s.dst = m
	return nil
}

// StreamForExport walks EVERY usage event in the [from,to] window in ONE
// snapshot transaction. See customer.PostgresStore.StreamForExport for the
// general reason (issue #475); usage is the case that made it urgent. This
// table is the hottest write path in the product, and the export is given a
// five-minute timeout PRECISELY because it streams a lot of rows — which is
// exactly the window in which concurrent ingest is guaranteed. Under the old
// LIMIT/OFFSET-per-transaction paging, every event ingested mid-export shifted
// the newest-first window and duplicated the row on a page boundary. A
// duplicated usage row in a finance CSV reads as usage that did not happen.
//
// The SELECT carries `origin`, which List's does not — so the `origin` column
// in usage-events.csv was always empty, and an operator could not tell an
// operator BACKFILL from a metered API event in the artifact they reconcile
// with. The export's columns are its own contract.
func (s *PostgresStore) StreamForExport(ctx context.Context, tenantID string, from, to time.Time, fn func(domain.UsageEvent) error) error {
	return s.StreamForExportFiltered(ctx, tenantID, from, to, ExportFilter{}, fn)
}

// ExportFilter narrows the usage export to what the Usage page shows:
// one customer, one meter, and events whose dimensions contain every pair
// (`properties @>`, the same match List uses). Empty fields do not filter.
type ExportFilter struct {
	CustomerID string
	MeterID    string
	Dimensions map[string]any
}

// StreamForExportFiltered is StreamForExport with the Usage page's filters,
// so "Export CSV" downloads exactly the rows the page lists.
func (s *PostgresStore) StreamForExportFiltered(ctx context.Context, tenantID string, from, to time.Time, f ExportFilter, fn func(domain.UsageEvent) error) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	// Tenant + livemode isolation is RLS's (TxTenant), as in List. The window is
	// REQUIRED by the caller (the table is unbounded), so both bounds always bind.
	where := "WHERE timestamp >= $1 AND timestamp <= $2"
	args := []any{from, to}
	if f.CustomerID != "" {
		args = append(args, f.CustomerID)
		where += fmt.Sprintf(" AND customer_id = $%d", len(args))
	}
	if f.MeterID != "" {
		args = append(args, f.MeterID)
		where += fmt.Sprintf(" AND meter_id = $%d", len(args))
	}
	if len(f.Dimensions) > 0 {
		doc, err := json.Marshal(f.Dimensions)
		if err != nil {
			return fmt.Errorf("export dimensions: %w", err)
		}
		args = append(args, string(doc))
		where += fmt.Sprintf(" AND properties @> $%d::jsonb", len(args))
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, tenant_id, customer_id, meter_id,
			quantity, properties, COALESCE(idempotency_key,''), timestamp, origin,
			provider_cost_micros, COALESCE(provider_cost_source,'')
		FROM usage_events
		`+where+`
		ORDER BY timestamp DESC, id DESC`, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var e domain.UsageEvent
		if err := rows.Scan(&e.ID, &e.TenantID, &e.CustomerID, &e.MeterID,
			&e.Quantity, propertiesScanner{&e.Dimensions},
			&e.IdempotencyKey, &e.Timestamp, &e.Origin,
			&e.ProviderCostMicros, &e.ProviderCostSource); err != nil {
			return err
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return rows.Err()
}
