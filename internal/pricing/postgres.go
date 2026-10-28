package pricing

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

type PostgresStore struct {
	db *postgres.DB
}

func NewPostgresStore(db *postgres.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// ---------------------------------------------------------------------------
// Meters
// ---------------------------------------------------------------------------

const meterColumns = `id, tenant_id, event_name, name, unit, aggregation, created_at, updated_at`

func scanMeter(row rowScanner) (domain.Meter, error) {
	var m domain.Meter
	err := row.Scan(&m.ID, &m.TenantID, &m.EventName, &m.Name, &m.Unit, &m.Aggregation, &m.CreatedAt, &m.UpdatedAt)
	if err == sql.ErrNoRows {
		return domain.Meter{}, errs.ErrNotFound
	}
	return m, err
}

func (s *PostgresStore) CreateMeter(ctx context.Context, tenantID string, m domain.Meter) (domain.Meter, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Meter{}, err
	}
	defer postgres.Rollback(tx)

	created, err := s.createMeterTx(ctx, tx, tenantID, m)
	if err != nil {
		return domain.Meter{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Meter{}, err
	}
	return created, nil
}

// CreateMeterTx inserts a meter inside an existing tx: the recipe install
// creates its meters, plans and prices on one transaction.
func (s *PostgresStore) CreateMeterTx(ctx context.Context, tx *sql.Tx, tenantID string, m domain.Meter) (domain.Meter, error) {
	return s.createMeterTx(ctx, tx, tenantID, m)
}

func (s *PostgresStore) createMeterTx(ctx context.Context, tx *sql.Tx, tenantID string, m domain.Meter) (domain.Meter, error) {
	id := postgres.NewID("hc_mtr")
	now := time.Now().UTC()
	created, err := scanMeter(tx.QueryRowContext(ctx, `
		INSERT INTO meters (id, tenant_id, event_name, name, unit, aggregation, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$7)
		RETURNING `+meterColumns,
		id, tenantID, m.EventName, m.Name, m.Unit, m.Aggregation, now))
	if err != nil {
		if postgres.IsUniqueViolation(err) {
			return domain.Meter{}, errs.AlreadyExists("event_name", fmt.Sprintf("a meter with event_name %q already exists", m.EventName))
		}
		return domain.Meter{}, err
	}
	return created, nil
}

func (s *PostgresStore) GetMeter(ctx context.Context, tenantID, id string) (domain.Meter, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Meter{}, err
	}
	defer postgres.Rollback(tx)
	return s.GetMeterTx(ctx, tx, id)
}

// GetMeterTx reads one meter on the caller's transaction, which carries the
// tenant. It takes no lock (LockMeterTx locks).
func (s *PostgresStore) GetMeterTx(ctx context.Context, tx *sql.Tx, id string) (domain.Meter, error) {
	return scanMeter(tx.QueryRowContext(ctx, `SELECT `+meterColumns+` FROM meters WHERE id = $1`, id))
}

func (s *PostgresStore) GetMeterByEventName(ctx context.Context, tenantID, eventName string) (domain.Meter, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Meter{}, err
	}
	defer postgres.Rollback(tx)
	return scanMeter(tx.QueryRowContext(ctx, `SELECT `+meterColumns+` FROM meters WHERE event_name = $1`, eventName))
}

// GetMeterByEventNameTx reads the meter on tx FOR KEY SHARE.
func (s *PostgresStore) GetMeterByEventNameTx(ctx context.Context, tx *sql.Tx, eventName string) (domain.Meter, error) {
	return scanMeter(tx.QueryRowContext(ctx, `SELECT `+meterColumns+` FROM meters WHERE event_name = $1 FOR KEY SHARE`, eventName))
}

// ListMeters returns every meter of the tenant, newest first, for internal
// callers that need them all. The API pages with ListMetersPage.
func (s *PostgresStore) ListMeters(ctx context.Context, tenantID string) ([]domain.Meter, error) {
	meters, _, err := s.listMeters(ctx, tenantID, 0, 0)
	return meters, err
}

// ListMetersPage returns one page of the tenant's meters, newest first, and
// how many meters the tenant has.
func (s *PostgresStore) ListMetersPage(ctx context.Context, tenantID string, limit, offset int) ([]domain.Meter, int, error) {
	return s.listMeters(ctx, tenantID, limit, offset)
}

// listMeters reads meters newest first; limit 0 reads them all and skips the
// count.
func (s *PostgresStore) listMeters(ctx context.Context, tenantID string, limit, offset int) ([]domain.Meter, int, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, 0, err
	}
	defer postgres.Rollback(tx)

	query := `SELECT ` + meterColumns + ` FROM meters ORDER BY created_at DESC, id DESC`
	var args []any
	total := 0
	if limit > 0 {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM meters`).Scan(&total); err != nil {
			return nil, 0, err
		}
		query += ` LIMIT $1 OFFSET $2`
		args = append(args, limit, offset)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	var meters []domain.Meter
	for rows.Next() {
		m, err := scanMeter(rows)
		if err != nil {
			return nil, 0, err
		}
		meters = append(meters, m)
	}
	return meters, total, rows.Err()
}

// UpdateMeterAudited patches the meter and runs the caller-supplied audit
// emission on the SAME transaction (ADR-090 shared fate). The emission sees
// the RETURNING row and only ever runs when the UPDATE matched a row: a PATCH
// against a meter that doesn't exist (or lives on the other livemode plane /
// another tenant under RLS) returns ErrNotFound with no audit row. An emit
// error aborts the patch. A non-nil guard runs first on the same tx; its
// error aborts the patch before anything is written.
func (s *PostgresStore) UpdateMeterAudited(ctx context.Context, tenantID string, m domain.Meter, guard func(tx *sql.Tx) error, emit func(tx *sql.Tx, out domain.Meter) error) (domain.Meter, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Meter{}, err
	}
	defer postgres.Rollback(tx)
	if guard != nil {
		if err := guard(tx); err != nil {
			return domain.Meter{}, err
		}
	}

	out, err := scanMeter(tx.QueryRowContext(ctx, `
		UPDATE meters SET name = $1, unit = $2, aggregation = $3, updated_at = $4
		WHERE id = $5
		RETURNING `+meterColumns,
		m.Name, m.Unit, m.Aggregation, time.Now().UTC(), m.ID))
	if err != nil {
		return domain.Meter{}, err
	}
	if emit != nil {
		if err := emit(tx, out); err != nil {
			return domain.Meter{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.Meter{}, err
	}
	return out, nil
}

// LockMeterTx locks the meter row and returns it as committed now. After
// waiting on a concurrent edit, FOR UPDATE returns the row that edit wrote.
func (s *PostgresStore) LockMeterTx(ctx context.Context, tx *sql.Tx, meterID string) (domain.Meter, error) {
	m, err := scanMeter(tx.QueryRowContext(ctx, `SELECT `+meterColumns+` FROM meters WHERE id = $1 FOR UPDATE`, meterID))
	if err != nil && err != errs.ErrNotFound {
		return domain.Meter{}, fmt.Errorf("lock meter %s: %w", meterID, err)
	}
	return m, err
}

// meterHasUsageSQL reports whether the meter has usage events (rows in
// usage_events), which fix its aggregation (ADR-130 §2). Held rows
// (usage_held_events) do not: they have never been billed, and a release
// writes usage_events, whose foreign key takes the meter lock an ingest
// takes. Row-level security scopes the read to the tenant and the mode;
// tenant_id leads the table's indexes. No index leads with (tenant_id,
// meter_id), so proving a meter has no events scans the tenant's events
// (ADR-130 §18).
const meterHasUsageSQL = `SELECT EXISTS (SELECT 1 FROM usage_events WHERE tenant_id = $1 AND meter_id = $2)`

// MeterHasUsageTx reads meterHasUsageSQL on the caller's transaction. After
// LockMeterTx it sees every event whose insert the lock waited for.
func (s *PostgresStore) MeterHasUsageTx(ctx context.Context, tx *sql.Tx, m domain.Meter) (bool, error) {
	var has bool
	if err := tx.QueryRowContext(ctx, meterHasUsageSQL, m.TenantID, m.ID).Scan(&has); err != nil {
		return false, fmt.Errorf("read whether meter %s has usage: %w", m.ID, err)
	}
	return has, nil
}

// MeterHasUsage reports whether the meter has usage events (meterHasUsageSQL).
func (s *PostgresStore) MeterHasUsage(ctx context.Context, tenantID string, m domain.Meter) (bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return false, err
	}
	defer postgres.Rollback(tx)
	var has bool
	if err := tx.QueryRowContext(ctx, meterHasUsageSQL, tenantID, m.ID).Scan(&has); err != nil {
		return false, fmt.Errorf("read whether meter %s has usage: %w", m.ID, err)
	}
	return has, nil
}

// ---------------------------------------------------------------------------
// Plans
// ---------------------------------------------------------------------------

const planColumns = `id, tenant_id, code, name, COALESCE(description,''), currency, billing_interval,
	status, base_amount_cents, created_at, updated_at, tax_code, base_bill_timing`

func scanPlan(row rowScanner) (domain.Plan, error) {
	var p domain.Plan
	err := row.Scan(&p.ID, &p.TenantID, &p.Code, &p.Name, &p.Description, &p.Currency,
		&p.BillingInterval, &p.Status, &p.BaseAmountCents,
		&p.CreatedAt, &p.UpdatedAt, &p.TaxCode, &p.BaseBillTiming)
	if err == sql.ErrNoRows {
		return domain.Plan{}, errs.ErrNotFound
	}
	return p, err
}

// CreatePlan creates a plan with no prices.
func (s *PostgresStore) CreatePlan(ctx context.Context, tenantID string, p domain.Plan) (domain.Plan, error) {
	created, _, err := s.CreatePlanWithPrices(ctx, tenantID, p, nil)
	return created, err
}

// CreatePlanWithPrices creates the plan and its prices in one transaction
// (ADR-130 §16). A new plan has never been used, so each price takes one
// version from the beginning of time.
func (s *PostgresStore) CreatePlanWithPrices(ctx context.Context, tenantID string, p domain.Plan, prices []PriceChange) (domain.Plan, []domain.PlanPrice, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Plan{}, nil, err
	}
	defer postgres.Rollback(tx)

	created, err := s.createPlanTx(ctx, tx, tenantID, p)
	if err != nil {
		return domain.Plan{}, nil, err
	}
	out, err := s.WritePricesTx(ctx, tx, created.ID, prices)
	if err != nil {
		return domain.Plan{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Plan{}, nil, err
	}
	return created, out, nil
}

// CreatePlanTx inserts a plan inside an existing tx (the recipe install).
func (s *PostgresStore) CreatePlanTx(ctx context.Context, tx *sql.Tx, tenantID string, p domain.Plan) (domain.Plan, error) {
	return s.createPlanTx(ctx, tx, tenantID, p)
}

func (s *PostgresStore) createPlanTx(ctx context.Context, tx *sql.Tx, tenantID string, p domain.Plan) (domain.Plan, error) {
	id := postgres.NewID("hc_pln")
	now := time.Now().UTC()
	if p.BaseBillTiming == "" {
		p.BaseBillTiming = domain.BillInArrears
	}
	created, err := scanPlan(tx.QueryRowContext(ctx, `
		INSERT INTO plans (id, tenant_id, code, name, description, currency, billing_interval,
			status, base_amount_cents, created_at, updated_at, tax_code, base_bill_timing)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10,$11,$12)
		RETURNING `+planColumns,
		id, tenantID, p.Code, p.Name, postgres.NullableString(p.Description),
		p.Currency, p.BillingInterval, p.Status, p.BaseAmountCents, now, p.TaxCode, p.BaseBillTiming))
	if err != nil {
		if postgres.IsUniqueViolation(err) {
			return domain.Plan{}, errs.AlreadyExists("code", fmt.Sprintf("plan code %q already exists", p.Code))
		}
		return domain.Plan{}, err
	}
	return created, nil
}

func (s *PostgresStore) GetPlan(ctx context.Context, tenantID, id string) (domain.Plan, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Plan{}, err
	}
	defer postgres.Rollback(tx)
	return scanPlan(tx.QueryRowContext(ctx, `SELECT `+planColumns+` FROM plans WHERE id = $1`, id))
}

// ListPlans returns every plan of the tenant, newest first, for internal
// callers that need them all. The API pages with ListPlansPage.
func (s *PostgresStore) ListPlans(ctx context.Context, tenantID string) ([]domain.Plan, error) {
	plans, _, err := s.listPlans(ctx, tenantID, 0, 0)
	return plans, err
}

// ListPlansPage returns one page of the tenant's plans, newest first, and how
// many plans the tenant has.
func (s *PostgresStore) ListPlansPage(ctx context.Context, tenantID string, limit, offset int) ([]domain.Plan, int, error) {
	return s.listPlans(ctx, tenantID, limit, offset)
}

// listPlans reads plans newest first; limit 0 reads them all and skips the
// count.
func (s *PostgresStore) listPlans(ctx context.Context, tenantID string, limit, offset int) ([]domain.Plan, int, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, 0, err
	}
	defer postgres.Rollback(tx)

	query := `SELECT ` + planColumns + ` FROM plans ORDER BY created_at DESC, id DESC`
	var args []any
	total := 0
	if limit > 0 {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM plans`).Scan(&total); err != nil {
			return nil, 0, err
		}
		query += ` LIMIT $1 OFFSET $2`
		args = append(args, limit, offset)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	var plans []domain.Plan
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, 0, err
		}
		plans = append(plans, p)
	}
	return plans, total, rows.Err()
}

// UpdatePlan locks the plan FOR UPDATE, as a writer of plan content (ADR-130
// §8 step 2), reads whether it was ever used, and writes what guard returns.
// A subscription writer referencing the plan takes it FOR KEY SHARE, so it
// waits for this write, and this write waits for it: "used" is read after
// the lock and cannot change before the commit.
func (s *PostgresStore) UpdatePlan(ctx context.Context, tenantID, id string, guard func(locked domain.Plan, used bool) (domain.Plan, error)) (domain.Plan, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Plan{}, err
	}
	defer postgres.Rollback(tx)

	locked, err := lockPlanTx(ctx, tx, id)
	if err != nil {
		return domain.Plan{}, err
	}
	used, err := planUsedTx(ctx, tx, id)
	if err != nil {
		return domain.Plan{}, err
	}
	p, err := guard(locked, used)
	if err != nil {
		return domain.Plan{}, err
	}
	out, err := scanPlan(tx.QueryRowContext(ctx, `
		UPDATE plans SET name = $1, description = $2, status = $3, base_amount_cents = $4,
			updated_at = $5, tax_code = $6, base_bill_timing = $7
		WHERE id = $8
		RETURNING `+planColumns,
		p.Name, postgres.NullableString(p.Description), p.Status, p.BaseAmountCents,
		time.Now().UTC(), p.TaxCode, p.BaseBillTiming, p.ID))
	if err != nil {
		return domain.Plan{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Plan{}, err
	}
	return out, nil
}

// PlanUsed reports whether any subscription ever referenced the plan.
func (s *PostgresStore) PlanUsed(ctx context.Context, tenantID, planID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return false, err
	}
	defer postgres.Rollback(tx)
	return planUsedTx(ctx, tx, planID)
}

// lockPlanTx takes the plan row FOR UPDATE: a writer of plan content
// (ADR-130 §8 step 2).
func lockPlanTx(ctx context.Context, tx *sql.Tx, planID string) (domain.Plan, error) {
	p, err := scanPlan(tx.QueryRowContext(ctx, `SELECT `+planColumns+` FROM plans WHERE id = $1 FOR UPDATE`, planID))
	if err != nil && err != errs.ErrNotFound {
		return domain.Plan{}, fmt.Errorf("lock plan %s: %w", planID, err)
	}
	return p, err
}

// planUsedSQL is the definition of a used plan (ADR-130 §5): a subscription
// item ever named it, as its plan or its pending plan, deleted items
// included, or a billing interval names it. Every insert of such a row, and
// every change of those columns, holds the plan FOR KEY SHARE through its
// foreign key, so after lockPlanTx this statement sees every such row that
// committed first, and a later one waits for the lock holder.
const planUsedSQL = `
	SELECT EXISTS (SELECT 1 FROM subscription_items WHERE plan_id = $1 OR pending_plan_id = $1)
	    OR EXISTS (SELECT 1 FROM billing_intervals WHERE plan_id = $1)`

func planUsedTx(ctx context.Context, tx *sql.Tx, planID string) (bool, error) {
	var used bool
	if err := tx.QueryRowContext(ctx, planUsedSQL, planID).Scan(&used); err != nil {
		return false, fmt.Errorf("read whether plan %s was used: %w", planID, err)
	}
	return used, nil
}

// ---------------------------------------------------------------------------
// Scan helpers
// ---------------------------------------------------------------------------

type rowScanner interface {
	Scan(dest ...any) error
}
