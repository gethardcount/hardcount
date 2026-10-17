package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// HeldStore keeps usage rows that could not be ingested yet because their
// customer or meter did not exist (#1044, ADR-122). PostgresStore
// implements it; NewService picks it up from the Store it is given.
type HeldStore interface {
	// Hold inserts a held row. Returns errs.ErrDuplicateKey (nothing
	// written) when a held row with the same idempotency key exists.
	Hold(ctx context.Context, tenantID string, ev domain.HeldUsageEvent) (domain.HeldUsageEvent, error)
	// ListHeldSummary groups the rows still held, for the most recent
	// HeldListCustomers external customer ids, with totals over all of them.
	ListHeldSummary(ctx context.Context, tenantID string) (HeldSummary, error)
	// HeldFor returns the distinct meter keys and the row count still held
	// for one external customer id.
	HeldFor(ctx context.Context, tenantID, externalCustomerID string) (meterKeys []string, rows int, err error)
	// ReleaseHeld moves held rows into usage_events in one transaction.
	ReleaseHeld(ctx context.Context, tenantID string, in ReleaseHeldInput, emit func(tx *sql.Tx, res ReleaseHeldResult) error) (ReleaseHeldResult, error)
	// DiscardHeld marks held rows discarded (kept, never ingested).
	DiscardHeld(ctx context.Context, tenantID, externalCustomerID, meterKey string, emit func(tx *sql.Tx, n int) error) (int, error)
	// IngestResolvingHeld is the live LiteLLM ingest: it writes the event
	// unless a held row with the same key was discarded, and resolves a
	// held row with the same key as released.
	IngestResolvingHeld(ctx context.Context, tenantID string, event domain.UsageEvent) (domain.UsageEvent, error)
}

// HeldListCustomers caps how many external customer ids
// GET /v1/usage-events/held lists. Every group of a listed id is returned,
// so a listed id's row count is complete; HeldSummary's totals count all ids.
const HeldListCustomers = 200

// HeldSummary is the held list plus totals over every row still held, so
// the dashboard can say when the list is cut.
type HeldSummary struct {
	Groups []HeldUsageGroup
	// TotalCustomers counts the distinct external customer ids with rows
	// still held, listed or not.
	TotalCustomers int
	// TotalRows counts every row still held, listed or not.
	TotalRows int
}

// HeldUsageGroup is one line of GET /v1/usage-events/held: the rows still
// held for one (external customer id, meter key, reason).
type HeldUsageGroup struct {
	ExternalCustomerID string                 `json:"external_customer_id"`
	MeterKey           string                 `json:"meter_key"`
	Reason             domain.HeldUsageReason `json:"reason"`
	Rows               int                    `json:"rows"`
	TotalQuantity      decimal.Decimal        `json:"total_quantity"`
	FirstEventAt       time.Time              `json:"first_event_at"`
	LastEventAt        time.Time              `json:"last_event_at"`
	LastReceivedAt     time.Time              `json:"last_received_at"`
	// CustomerID is the customer whose external_id now matches, or ""
	// when none does yet. Filled by the handler, not the store.
	CustomerID string `json:"customer_id"`
	// MeterExists says whether a meter with MeterKey exists now. Filled by
	// the handler, not the store.
	MeterExists bool `json:"meter_exists"`
}

// ReleaseHeldInput selects the held rows to release and where they go.
type ReleaseHeldInput struct {
	ExternalCustomerID string
	// CustomerID is the customer the rows are ingested for.
	CustomerID string
	// MeterIDs maps meter key to meter id. Rows whose meter key is absent
	// stay held.
	MeterIDs map[string]string
	// NotAfter is the customer's current time. Rows stamped after it stay
	// held: ingesting them would put usage in the customer's future.
	NotAfter time.Time
	// Limit caps the rows claimed by one call.
	Limit int
}

// ReleasedRow is one held row the release wrote, or found already written.
type ReleasedRow struct {
	HeldID       string    `json:"-"`
	EventID      string    `json:"-"`
	MeterKey     string    `json:"-"`
	EventAt      time.Time `json:"-"`
	Deduplicated bool      `json:"-"`
}

// ReleaseHeldResult reports what one release did.
type ReleaseHeldResult struct {
	Rows []ReleasedRow
	// StillHeld counts rows for the external customer id that are still
	// held after this release and that this release could not take: a meter
	// that does not exist, or a time after the customer's current time.
	StillHeld int
	// MoreToRelease counts rows still held that this release could have
	// taken but did not, because it stopped at Limit. Another release takes
	// them.
	MoreToRelease int
}

// Hold inserts one held row. ON CONFLICT DO NOTHING on the idempotency key,
// so a LiteLLM resend of a row already held writes nothing and reports
// errs.ErrDuplicateKey, the same contract ingestOneTx has.
func (s *PostgresStore) Hold(ctx context.Context, tenantID string, ev domain.HeldUsageEvent) (domain.HeldUsageEvent, error) {
	props, err := propertiesJSON(ev.Dimensions)
	if err != nil {
		return domain.HeldUsageEvent{}, err
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.HeldUsageEvent{}, err
	}
	defer postgres.Rollback(tx)

	err = tx.QueryRowContext(ctx, `
		INSERT INTO usage_held_events (id, tenant_id, source, external_customer_id,
			meter_key, quantity, properties, idempotency_key, event_at,
			observed_cost_micros, reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (tenant_id, livemode, idempotency_key) DO NOTHING
		RETURNING id, received_at, status
	`, postgres.NewID("vlx_hue"), tenantID, ev.Source, ev.ExternalCustomerID,
		ev.MeterKey, ev.Quantity, props, ev.IdempotencyKey, ev.EventAt,
		ev.ObservedCostMicros, string(ev.Reason),
	).Scan(&ev.ID, &ev.ReceivedAt, &ev.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.HeldUsageEvent{}, fmt.Errorf("%w: held idempotency_key %q", errs.ErrDuplicateKey, ev.IdempotencyKey)
	}
	if err != nil {
		return domain.HeldUsageEvent{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.HeldUsageEvent{}, err
	}
	return ev, nil
}

// ListHeldSummary returns one group per (external customer id, meter key,
// reason) over the rows still held, newest arrival first.
func (s *PostgresStore) ListHeldSummary(ctx context.Context, tenantID string) (HeldSummary, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return HeldSummary{}, err
	}
	defer postgres.Rollback(tx)

	out := HeldSummary{Groups: []HeldUsageGroup{}}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT external_customer_id), COUNT(*)
		FROM usage_held_events
		WHERE tenant_id = $1 AND status = 'held'
	`, tenantID).Scan(&out.TotalCustomers, &out.TotalRows); err != nil {
		return HeldSummary{}, err
	}

	// The cap is on external customer ids, not groups, so every listed id
	// carries all its held rows.
	rows, err := tx.QueryContext(ctx, `
		WITH listed AS (
			SELECT external_customer_id
			FROM usage_held_events
			WHERE tenant_id = $1 AND status = 'held'
			GROUP BY external_customer_id
			ORDER BY MAX(received_at) DESC, external_customer_id
			LIMIT $2
		)
		SELECT external_customer_id, meter_key, reason, COUNT(*), SUM(quantity),
			MIN(event_at), MAX(event_at), MAX(received_at)
		FROM usage_held_events
		WHERE tenant_id = $1 AND status = 'held'
		  AND external_customer_id IN (SELECT external_customer_id FROM listed)
		GROUP BY external_customer_id, meter_key, reason
		ORDER BY MAX(received_at) DESC, external_customer_id, meter_key
	`, tenantID, HeldListCustomers)
	if err != nil {
		return HeldSummary{}, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var g HeldUsageGroup
		if err := rows.Scan(&g.ExternalCustomerID, &g.MeterKey, &g.Reason, &g.Rows, &g.TotalQuantity,
			&g.FirstEventAt, &g.LastEventAt, &g.LastReceivedAt); err != nil {
			return HeldSummary{}, err
		}
		out.Groups = append(out.Groups, g)
	}
	return out, rows.Err()
}

// HeldFor returns the meter keys and row count still held for one external
// customer id.
func (s *PostgresStore) HeldFor(ctx context.Context, tenantID, externalCustomerID string) ([]string, int, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, 0, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT meter_key, COUNT(*) FROM usage_held_events
		WHERE tenant_id = $1 AND status = 'held' AND external_customer_id = $2
		GROUP BY meter_key ORDER BY meter_key
	`, tenantID, externalCustomerID)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	var keys []string
	total := 0
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, 0, err
		}
		keys = append(keys, k)
		total += n
	}
	return keys, total, rows.Err()
}

// ReleaseHeld ingests held rows for one external customer id in ONE
// transaction:
//
//  1. Claim: UPDATE ... SET status='released' WHERE status='held' ...
//     RETURNING. Row locks make a concurrent release wait, then re-check
//     status='held' and skip what this one claimed: disjoint sets.
//  2. Insert each claimed row through ingestOneTx, the same INSERT every
//     ingest path uses, under its ORIGINAL idempotency key and event time,
//     with origin 'backfill'. ON CONFLICT there means usage_events already
//     has the key (LiteLLM resent the call after the customer existed): the
//     held row is still marked released, with no event id, and nothing is
//     billed twice.
//  3. Record the written event id on the held row, run the audit emission,
//     commit. A failure anywhere rolls all of it back: the rows stay held.
func (s *PostgresStore) ReleaseHeld(ctx context.Context, tenantID string, in ReleaseHeldInput, emit func(tx *sql.Tx, res ReleaseHeldResult) error) (ReleaseHeldResult, error) {
	keys := make([]string, 0, len(in.MeterIDs))
	for k := range in.MeterIDs {
		keys = append(keys, k)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 5000
	}

	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return ReleaseHeldResult{}, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		UPDATE usage_held_events SET status = 'released', resolved_at = now(),
			released_customer_id = $5
		WHERE id IN (
			SELECT id FROM usage_held_events
			WHERE tenant_id = $1 AND status = 'held'
			  AND external_customer_id = $2
			  AND meter_key = ANY($3::text[])
			  AND event_at <= $4
			ORDER BY event_at, id
			LIMIT $6
		)
		AND status = 'held'
		RETURNING id, meter_key, quantity, properties, idempotency_key, event_at, observed_cost_micros
	`, tenantID, in.ExternalCustomerID, postgres.StringArray(keys), in.NotAfter, in.CustomerID, limit)
	if err != nil {
		return ReleaseHeldResult{}, err
	}
	var claimed []domain.HeldUsageEvent
	for rows.Next() {
		var h domain.HeldUsageEvent
		if err := rows.Scan(&h.ID, &h.MeterKey, &h.Quantity, propertiesScanner{&h.Dimensions},
			&h.IdempotencyKey, &h.EventAt, &h.ObservedCostMicros); err != nil {
			_ = rows.Close()
			return ReleaseHeldResult{}, err
		}
		claimed = append(claimed, h)
	}
	if err := rows.Close(); err != nil {
		return ReleaseHeldResult{}, err
	}
	if err := rows.Err(); err != nil {
		return ReleaseHeldResult{}, err
	}

	res := ReleaseHeldResult{Rows: make([]ReleasedRow, 0, len(claimed))}
	for _, h := range claimed {
		out, err := ingestOneTx(ctx, tx, tenantID, domain.UsageEvent{
			CustomerID:         in.CustomerID,
			MeterID:            in.MeterIDs[h.MeterKey],
			Quantity:           h.Quantity,
			Dimensions:         h.Dimensions,
			IdempotencyKey:     h.IdempotencyKey,
			Timestamp:          h.EventAt,
			Origin:             domain.UsageOriginBackfill,
			ProviderCostMicros: h.ObservedCostMicros,
		})
		row := ReleasedRow{HeldID: h.ID, MeterKey: h.MeterKey, EventAt: h.EventAt}
		switch {
		case errors.Is(err, errs.ErrDuplicateKey):
			row.Deduplicated = true
		case err != nil:
			return ReleaseHeldResult{}, fmt.Errorf("release held %s: %w", h.ID, err)
		default:
			row.EventID = out.ID
			if _, err := tx.ExecContext(ctx,
				`UPDATE usage_held_events SET released_event_id = $1 WHERE id = $2`, out.ID, h.ID); err != nil {
				return ReleaseHeldResult{}, err
			}
		}
		res.Rows = append(res.Rows, row)
	}

	// Split what is left: rows this release could have taken but for the
	// Limit (another release takes them) and rows it could not take.
	if err := tx.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE meter_key = ANY($3::text[]) AND event_at <= $4),
			COUNT(*) FILTER (WHERE NOT (meter_key = ANY($3::text[]) AND event_at <= $4))
		FROM usage_held_events
		WHERE tenant_id = $1 AND status = 'held' AND external_customer_id = $2
	`, tenantID, in.ExternalCustomerID, postgres.StringArray(keys), in.NotAfter).Scan(&res.MoreToRelease, &res.StillHeld); err != nil {
		return ReleaseHeldResult{}, err
	}

	if emit != nil && len(res.Rows) > 0 {
		if err := emit(tx, res); err != nil {
			return ReleaseHeldResult{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return ReleaseHeldResult{}, err
	}
	return res, nil
}

// DiscardHeld marks the held rows for one external customer id (and one
// meter key, when given) discarded. The rows are kept as a record; they are
// never ingested.
func (s *PostgresStore) DiscardHeld(ctx context.Context, tenantID, externalCustomerID, meterKey string, emit func(tx *sql.Tx, n int) error) (int, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return 0, err
	}
	defer postgres.Rollback(tx)

	res, err := tx.ExecContext(ctx, `
		UPDATE usage_held_events SET status = 'discarded', resolved_at = now()
		WHERE tenant_id = $1 AND status = 'held' AND external_customer_id = $2
		  AND ($3::text = '' OR meter_key = $3::text)
	`, tenantID, externalCustomerID, meterKey)
	if err != nil {
		return 0, err
	}
	n64, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	n := int(n64)
	if emit != nil && n > 0 {
		if err := emit(tx, n); err != nil {
			return 0, fmt.Errorf("audit emission: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// IngestResolvingHeld writes one live LiteLLM event, in one transaction
// with the held row that has the same idempotency key, if any:
//
//   - discarded: nothing is written and errs.ErrDuplicateKey is returned.
//     The operator decided these rows are never billed, and a LiteLLM
//     resend after the customer exists must not undo that.
//   - held: the event is written (or found already written) and the held
//     row is marked released, so a later discard cannot mark a row
//     discarded whose usage is already in usage_events.
//   - released, or no held row: a plain ingest.
//
// SELECT ... FOR UPDATE takes the held row's lock, the same lock
// ReleaseHeld's and DiscardHeld's UPDATEs take, so whichever runs second
// sees the first one's status.
func (s *PostgresStore) IngestResolvingHeld(ctx context.Context, tenantID string, event domain.UsageEvent) (domain.UsageEvent, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.UsageEvent{}, err
	}
	defer postgres.Rollback(tx)

	var heldID, status string
	if event.IdempotencyKey != "" {
		err = tx.QueryRowContext(ctx, `
			SELECT id, status FROM usage_held_events
			WHERE tenant_id = $1 AND idempotency_key = $2
			FOR UPDATE
		`, tenantID, event.IdempotencyKey).Scan(&heldID, &status)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return domain.UsageEvent{}, err
		}
	}
	if status == string(domain.HeldStatusDiscarded) {
		return domain.UsageEvent{}, fmt.Errorf("%w: idempotency_key %q was held and discarded", errs.ErrDuplicateKey, event.IdempotencyKey)
	}

	out, ingestErr := ingestOneTx(ctx, tx, tenantID, event)
	if ingestErr != nil && !errors.Is(ingestErr, errs.ErrDuplicateKey) {
		return domain.UsageEvent{}, ingestErr
	}
	if status == string(domain.HeldStatusHeld) {
		// The event id is recorded only when this call wrote it; a
		// duplicate means usage_events already had the key.
		var eventID *string
		if ingestErr == nil {
			eventID = &out.ID
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE usage_held_events SET status = 'released', resolved_at = now(),
				released_customer_id = $2, released_event_id = $3
			WHERE id = $1 AND status = 'held'
		`, heldID, event.CustomerID, eventID); err != nil {
			return domain.UsageEvent{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.UsageEvent{}, err
	}
	if ingestErr != nil {
		return domain.UsageEvent{}, ingestErr
	}
	return out, nil
}
