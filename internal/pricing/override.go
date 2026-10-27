package pricing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// Customer price overrides (ADR-130 §6). An override is keyed by (customer,
// price) and replaces the price's shape for that customer over
// [created_at, deactivated_at). A create, replace or delete stamps that
// window after the write's locks, in the customer's own time: wall time read
// with clock_timestamp(), or the customer's clock time plus 1 microsecond.
// Not now(): that is the transaction's start, so a write that waited for a
// lock across a period start would change the period already open. A plan
// is priced at its pin of the period (ADR-130 §4 step 3), so an override
// write applies from the customer's next period. A subscription whose first
// join of the plan in a period comes after the write gets it in that period.

const overrideColumns = `o.id, o.tenant_id, o.customer_id, o.price_id, o.mode, o.unit_amount_cents, o.tiers,
	o.package_size, o.package_amount_cents, COALESCE(o.reason, ''), o.active, o.created_at, o.deactivated_at, o.updated_at`

func scanOverride(row rowScanner) (domain.CustomerPriceOverride, error) {
	var (
		o     domain.CustomerPriceOverride
		tiers []byte
		deact sql.NullTime
	)
	err := row.Scan(&o.ID, &o.TenantID, &o.CustomerID, &o.PriceID, &o.Mode, &o.UnitAmountCents, &tiers,
		&o.PackageSize, &o.PackageAmountCents, &o.Reason, &o.Active, &o.CreatedAt, &deact, &o.UpdatedAt)
	if err == sql.ErrNoRows {
		return domain.CustomerPriceOverride{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.CustomerPriceOverride{}, err
	}
	if deact.Valid {
		t := deact.Time
		o.DeactivatedAt = &t
	}
	if len(tiers) > 0 {
		if err := json.Unmarshal(tiers, &o.Tiers); err != nil {
			return domain.CustomerPriceOverride{}, fmt.Errorf("decode tiers of override %s: %w", o.ID, err)
		}
	}
	return o, nil
}

// overrideLocksTx takes the locks an override write holds before it reads
// the customer's clock (ADR-130 §8): the price's plan FOR KEY SHARE, then the
// price FOR KEY SHARE. So it waits for a price write in progress on the
// plan, a removal included, and reads the price as that write left it. It
// returns whether the price takes part from now on.
func overrideLocksTx(ctx context.Context, tx *sql.Tx, priceID string) (taking bool, err error) {
	var planID string
	if err := tx.QueryRowContext(ctx, `SELECT plan_id FROM plan_prices WHERE id = $1`, priceID).Scan(&planID); err != nil {
		if err == sql.ErrNoRows {
			return false, errs.ErrNotFound
		}
		return false, fmt.Errorf("read price %s: %w", priceID, err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM plans WHERE id = $1 FOR KEY SHARE`, planID); err != nil {
		return false, fmt.Errorf("lock plan %s: %w", planID, err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM plan_prices WHERE id = $1 FOR KEY SHARE`, priceID); err != nil {
		return false, fmt.Errorf("lock price %s: %w", priceID, err)
	}
	var removed bool
	err = tx.QueryRowContext(ctx, `
		SELECT removed FROM plan_price_versions WHERE price_id = $1
		ORDER BY valid_from DESC LIMIT 1`, priceID).Scan(&removed)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read price %s: %w", priceID, err)
	}
	return !removed, nil
}

// customerStampTx is the instant an override write for the customer stamps:
// the customer's clock time plus 1 microsecond, its clock read FOR SHARE
// (ADR-130 §8 step 5) so a clock advance or teardown is ordered with the
// write; or wall time, clock_timestamp(), for a customer without a clock.
func customerStampTx(ctx context.Context, tx *sql.Tx, customerID string) (time.Time, error) {
	var clockID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT test_clock_id FROM customers WHERE id = $1`, customerID).Scan(&clockID); err != nil {
		if err == sql.ErrNoRows {
			return time.Time{}, errs.NotFound("customer")
		}
		return time.Time{}, fmt.Errorf("read customer %s: %w", customerID, err)
	}
	var at time.Time
	if clockID.Valid && clockID.String != "" {
		err := tx.QueryRowContext(ctx, `
			SELECT frozen_time + interval '1 microsecond' FROM test_clocks WHERE id = $1 FOR SHARE`, clockID.String).Scan(&at)
		if err == sql.ErrNoRows {
			// A teardown of the clock committed while this waited for it,
			// and deleted the customer with it.
			return time.Time{}, errs.NotFound("customer")
		}
		if err != nil {
			return time.Time{}, fmt.Errorf("lock test clock %s: %w", clockID.String, err)
		}
		return at, nil
	}
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&at); err != nil {
		return time.Time{}, err
	}
	return at, nil
}

// WriteOverride closes the customer's active override of the price, if any,
// and inserts o, both at the customer's stamp.
func (s *PostgresStore) WriteOverride(ctx context.Context, tenantID string, o domain.CustomerPriceOverride) (domain.CustomerPriceOverride, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.CustomerPriceOverride{}, err
	}
	defer postgres.Rollback(tx)

	taking, err := overrideLocksTx(ctx, tx, o.PriceID)
	if errors.Is(err, errs.ErrNotFound) {
		return domain.CustomerPriceOverride{}, errs.Invalid("price_id", fmt.Sprintf("price %q not found", o.PriceID))
	}
	if err != nil {
		return domain.CustomerPriceOverride{}, err
	}
	if !taking {
		return domain.CustomerPriceOverride{}, errs.InvalidState(fmt.Sprintf(
			"Price %s was removed from its plan, so it takes no part from now on. Override a price the plan has.", o.PriceID))
	}
	at, err := customerStampTx(ctx, tx, o.CustomerID)
	if err != nil {
		return domain.CustomerPriceOverride{}, err
	}

	// One active row per (customer, price): the prior window closes where
	// the new one opens. The partial unique index turns a concurrent double
	// write into a clean retryable conflict below.
	if _, err := tx.ExecContext(ctx, `
		UPDATE customer_price_overrides
		SET active = false, deactivated_at = $1, updated_at = now()
		WHERE customer_id = $2 AND price_id = $3 AND active`, at, o.CustomerID, o.PriceID); err != nil {
		return domain.CustomerPriceOverride{}, fmt.Errorf("close the prior override window: %w", err)
	}
	tiers, err := json.Marshal([]domain.RatingTier(o.Tiers))
	if err != nil {
		return domain.CustomerPriceOverride{}, fmt.Errorf("encode tiers: %w", err)
	}
	if o.Tiers == nil {
		tiers = []byte("[]")
	}
	created, err := scanOverride(tx.QueryRowContext(ctx, `
		INSERT INTO customer_price_overrides AS o (id, tenant_id, customer_id, price_id,
			mode, unit_amount_cents, tiers, package_size, package_amount_cents,
			reason, active, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, true, $11, now())
		RETURNING `+overrideColumns,
		postgres.NewID("vlx_cpo"), tenantID, o.CustomerID, o.PriceID,
		o.Mode, o.UnitAmountCents, tiers, o.PackageSize, o.PackageAmountCents,
		postgres.NullableString(o.Reason), at))
	if err != nil {
		if postgres.IsUniqueViolation(err) {
			return domain.CustomerPriceOverride{}, errs.AlreadyExists("price_id", "a concurrent write created this override — retry")
		}
		return domain.CustomerPriceOverride{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.CustomerPriceOverride{}, err
	}
	return created, nil
}

// DeactivateOverride ends a negotiated price at the customer's stamp: the
// period in flight keeps it, the next period is at list price. The row is
// kept. A second delete is a clean 404.
func (s *PostgresStore) DeactivateOverride(ctx context.Context, tenantID, id string) (domain.CustomerPriceOverride, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.CustomerPriceOverride{}, err
	}
	defer postgres.Rollback(tx)

	cur, err := scanOverride(tx.QueryRowContext(ctx, `SELECT `+overrideColumns+` FROM customer_price_overrides o WHERE o.id = $1 AND o.active`, id))
	if err != nil {
		return domain.CustomerPriceOverride{}, err
	}
	if _, err := overrideLocksTx(ctx, tx, cur.PriceID); err != nil {
		return domain.CustomerPriceOverride{}, err
	}
	at, err := customerStampTx(ctx, tx, cur.CustomerID)
	if err != nil {
		return domain.CustomerPriceOverride{}, err
	}
	out, err := scanOverride(tx.QueryRowContext(ctx, `
		UPDATE customer_price_overrides AS o
		SET active = false, deactivated_at = GREATEST(o.created_at, $1), updated_at = now()
		WHERE o.id = $2 AND o.active
		RETURNING `+overrideColumns, at, id))
	if err != nil {
		// A removal of the price ended it while this waited for the locks.
		return domain.CustomerPriceOverride{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.CustomerPriceOverride{}, err
	}
	return out, nil
}

// GetOverrideAt resolves the override of (customer, price) in force at `at`.
// Windows never overlap (each writer closes the prior window in the same
// transaction), so at most one row spans any instant. errs.ErrNotFound means
// list price; any other error is a real failure to every rating path, never
// absence.
func (s *PostgresStore) GetOverrideAt(ctx context.Context, tenantID, customerID, priceID string, at time.Time) (domain.CustomerPriceOverride, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.CustomerPriceOverride{}, err
	}
	defer postgres.Rollback(tx)
	return scanOverride(tx.QueryRowContext(ctx, `
		SELECT `+overrideColumns+`
		FROM customer_price_overrides o
		WHERE o.customer_id = $1 AND o.price_id = $2
		  AND o.created_at <= $3
		  AND (o.deactivated_at IS NULL OR o.deactivated_at > $3)
		ORDER BY o.created_at DESC LIMIT 1`, customerID, priceID, at))
}

// ListOverrides returns active overrides, of one customer and of one price
// when those are given.
func (s *PostgresStore) ListOverrides(ctx context.Context, tenantID, customerID, priceID string) ([]domain.CustomerPriceOverride, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	query := `SELECT ` + overrideColumns + ` FROM customer_price_overrides o WHERE o.active`
	var args []any
	if customerID != "" {
		args = append(args, customerID)
		query += fmt.Sprintf(" AND o.customer_id = $%d", len(args))
	}
	if priceID != "" {
		args = append(args, priceID)
		query += fmt.Sprintf(" AND o.price_id = $%d", len(args))
	}
	query += " ORDER BY o.created_at DESC LIMIT 500"
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []domain.CustomerPriceOverride
	for rows.Next() {
		o, err := scanOverride(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// CreateOverrideInput is POST /v1/price-overrides.
type CreateOverrideInput struct {
	CustomerID string `json:"customer_id"`
	PriceID    string `json:"price_id"`
	domain.PriceShape
	Reason string `json:"reason,omitempty"`
}

// CreateOverride sets the customer's negotiated price for one plan price,
// from the customer's next period. A second create for the same (customer,
// price) replaces the first from then on.
func (s *Service) CreateOverride(ctx context.Context, tenantID string, input CreateOverrideInput) (domain.CustomerPriceOverride, error) {
	if strings.TrimSpace(input.CustomerID) == "" {
		return domain.CustomerPriceOverride{}, errs.Required("customer_id")
	}
	if strings.TrimSpace(input.PriceID) == "" {
		return domain.CustomerPriceOverride{}, errs.Required("price_id")
	}
	// The customer must be the caller's. The foreign key on customer_id is on
	// the id alone and foreign-key checks do not apply row-level security, so
	// without this read the insert accepts any existing customer id,
	// including another tenant's or the other mode's.
	if s.customers != nil {
		if _, err := s.customers.Get(ctx, tenantID, input.CustomerID); err != nil {
			if errors.Is(err, errs.ErrNotFound) {
				return domain.CustomerPriceOverride{}, errs.NotFound("customer")
			}
			return domain.CustomerPriceOverride{}, fmt.Errorf("get customer: %w", err)
		}
	}
	shape, err := checkShape("", input.PriceShape)
	if err != nil {
		return domain.CustomerPriceOverride{}, err
	}
	if _, err := domain.ComputeAmountCents(shape, decimal.NewFromInt(1)); err != nil {
		return domain.CustomerPriceOverride{}, errs.Invalid("pricing", fmt.Sprintf("invalid pricing configuration: %v", err))
	}
	return s.store.WriteOverride(ctx, tenantID, domain.CustomerPriceOverride{
		CustomerID: input.CustomerID,
		PriceID:    input.PriceID,
		PriceShape: shape,
		Reason:     strings.TrimSpace(input.Reason),
	})
}

// DeleteOverride ends a negotiated price from the customer's next period.
func (s *Service) DeleteOverride(ctx context.Context, tenantID, id string) (domain.CustomerPriceOverride, error) {
	if strings.TrimSpace(id) == "" {
		return domain.CustomerPriceOverride{}, errs.Required("id")
	}
	return s.store.DeactivateOverride(ctx, tenantID, id)
}

// GetOverrideAt resolves the override in force at `at`: the rating paths'
// lookup.
func (s *Service) GetOverrideAt(ctx context.Context, tenantID, customerID, priceID string, at time.Time) (domain.CustomerPriceOverride, error) {
	return s.store.GetOverrideAt(ctx, tenantID, customerID, priceID, at)
}

// ListOverrides lists active overrides.
func (s *Service) ListOverrides(ctx context.Context, tenantID, customerID, priceID string) ([]domain.CustomerPriceOverride, error) {
	return s.store.ListOverrides(ctx, tenantID, customerID, priceID)
}
