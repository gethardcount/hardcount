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
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// Override methods on the existing PostgresStore.

func (s *PostgresStore) CreateOverride(ctx context.Context, tenantID string, o domain.CustomerPriceOverride) (domain.CustomerPriceOverride, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.CustomerPriceOverride{}, err
	}
	defer postgres.Rollback(tx)

	id := postgres.NewID("vlx_cpo")
	// Honors ctx-bound effective-now (ADR-030) — overrides on
	// clock-pinned customers stamp sim-time. Fallback to wall-clock
	// otherwise.
	now := clock.Now(ctx)
	tiersJSON, _ := json.Marshal(o.GraduatedTiers)

	// Append-only effectivity rows (ADR-070): re-issuing a key closes
	// the prior row's window (deactivated_at) and inserts a fresh one —
	// the one-active-row partial unique index (0128) turns a concurrent
	// double-write into a clean retryable conflict below.
	// [created_at, deactivated_at) is what the as-of lookup resolves —
	// a mid-period edit therefore prices from the NEXT period open; the
	// period in flight keeps the row that was live when it opened.
	// rating_rule_version_id records the version the operator
	// referenced (provenance, not identity).
	if _, err := tx.ExecContext(ctx, `
		UPDATE customer_price_overrides
		SET active = false, deactivated_at = $1, updated_at = $1
		WHERE customer_id = $2 AND rule_key = $3 AND active = true
	`, now, o.CustomerID, o.RuleKey); err != nil {
		return domain.CustomerPriceOverride{}, fmt.Errorf("close prior override window: %w", err)
	}

	err = tx.QueryRowContext(ctx, `
		INSERT INTO customer_price_overrides (id, tenant_id, customer_id, rule_key, rating_rule_version_id,
			mode, flat_amount_cents, graduated_tiers, package_size, package_amount_cents,
			overage_unit_amount_cents, reason, active, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$14)
		RETURNING id, tenant_id, customer_id, rule_key, rating_rule_version_id, mode,
			flat_amount_cents, graduated_tiers, package_size, package_amount_cents,
			overage_unit_amount_cents, COALESCE(reason,''), active, created_at, updated_at
	`, id, tenantID, o.CustomerID, o.RuleKey, o.RatingRuleVersionID,
		o.Mode, o.FlatAmountCents, tiersJSON, o.PackageSize,
		o.PackageAmountCents, o.OverageUnitAmountCents,
		postgres.NullableString(o.Reason), true, now,
	).Scan(&o.ID, &o.TenantID, &o.CustomerID, &o.RuleKey, &o.RatingRuleVersionID, &o.Mode,
		&o.FlatAmountCents, &tiersJSON, &o.PackageSize, &o.PackageAmountCents,
		&o.OverageUnitAmountCents, &o.Reason, &o.Active, &o.CreatedAt, &o.UpdatedAt)

	if err != nil {
		if postgres.IsUniqueViolation(err) {
			// Two concurrent upserts for the same key: the loser's
			// deactivate saw no row (the winner's wasn't committed) and
			// its insert hit the one-active-row partial unique. Rare
			// operator race — surface for a clean retry, never a
			// silent second live row.
			return domain.CustomerPriceOverride{}, errs.AlreadyExists("rule_key",
				"a concurrent write created this override — retry")
		}
		return domain.CustomerPriceOverride{}, err
	}
	if len(tiersJSON) > 0 {
		if err := json.Unmarshal(tiersJSON, &o.GraduatedTiers); err != nil {
			return domain.CustomerPriceOverride{}, fmt.Errorf("unmarshal graduated_tiers: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.CustomerPriceOverride{}, err
	}
	return o, nil
}

// GetOverrideByKeyAsOf resolves the override row in force at asOf for
// (customer, rule_key) — ADR-070: rule_key is the override's identity
// (a version publish never detaches it) and asOf is the billing
// period's open, so a mid-period upsert or deactivate prices from the
// NEXT period while the period in flight keeps the price it opened
// with. errs.ErrNotFound means "no override in force — list price";
// any other error must be treated as a real failure by rating paths,
// never as absence (no-silent-fallbacks).
func (s *PostgresStore) GetOverrideByKeyAsOf(ctx context.Context, tenantID, customerID, ruleKey string, asOf time.Time) (domain.CustomerPriceOverride, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.CustomerPriceOverride{}, err
	}
	defer postgres.Rollback(tx)

	var o domain.CustomerPriceOverride
	var tiersJSON []byte
	// Effectivity window: [created_at, deactivated_at). Post-0128,
	// windows never overlap (each writer closes the prior window in the
	// same tx), so at most one row spans any instant. The one exception
	// is the 0128 collapse itself: pre-migration duplicates all had
	// open-ended windows closed at the SAME migration instant, so for
	// an asOf inside that legacy span both winner and demoted rows
	// match — `active DESC` prefers the collapse winner there and is
	// inert everywhere else (an active row's created_at bounds it out
	// of any earlier window).
	err = tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, customer_id, rule_key, rating_rule_version_id, mode,
			flat_amount_cents, graduated_tiers, package_size, package_amount_cents,
			overage_unit_amount_cents, COALESCE(reason,''), active, created_at, updated_at
		FROM customer_price_overrides
		WHERE customer_id = $1 AND rule_key = $2
		  AND created_at <= $3
		  AND (deactivated_at IS NULL OR deactivated_at > $3)
		ORDER BY active DESC, created_at DESC LIMIT 1
	`, customerID, ruleKey, asOf).Scan(&o.ID, &o.TenantID, &o.CustomerID, &o.RuleKey, &o.RatingRuleVersionID,
		&o.Mode, &o.FlatAmountCents, &tiersJSON, &o.PackageSize, &o.PackageAmountCents,
		&o.OverageUnitAmountCents, &o.Reason, &o.Active, &o.CreatedAt, &o.UpdatedAt)

	if err == sql.ErrNoRows {
		return domain.CustomerPriceOverride{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.CustomerPriceOverride{}, err
	}
	if len(tiersJSON) > 0 {
		if err := json.Unmarshal(tiersJSON, &o.GraduatedTiers); err != nil {
			return domain.CustomerPriceOverride{}, fmt.Errorf("unmarshal graduated_tiers: %w", err)
		}
	}
	return o, nil
}

// DeactivateOverride soft-ends a negotiated price: the row flips
// active=false with its effectivity window closed at deactivated_at;
// the row is kept for audit and for as-of resolution of periods that
// opened while it was live (a period in flight still bills it; the
// next period resolves list price). CAS on active=true — a second
// DELETE is a clean 404.
func (s *PostgresStore) DeactivateOverride(ctx context.Context, tenantID, id string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	now := clock.Now(ctx)
	res, err := tx.ExecContext(ctx, `
		UPDATE customer_price_overrides
		SET active = false, deactivated_at = $1, updated_at = $1
		WHERE id = $2 AND active = true
	`, now, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errs.ErrNotFound
	}
	return tx.Commit()
}

// CountActiveOverridesByRuleKey backs the publish-time currency guard
// (ADR-070): a currency change within a rule_key is rejected while any
// customer's active override references the key — a rule_key-following
// override would silently reinterpret its integer cents in the new
// currency.
func (s *PostgresStore) CountActiveOverridesByRuleKey(ctx context.Context, tenantID, ruleKey string) (int, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return 0, err
	}
	defer postgres.Rollback(tx)

	var n int
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM customer_price_overrides
		WHERE rule_key = $1 AND active = true
	`, ruleKey).Scan(&n)
	return n, err
}

func (s *PostgresStore) ListOverrides(ctx context.Context, tenantID, customerID string) ([]domain.CustomerPriceOverride, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	query := `SELECT id, tenant_id, customer_id, rule_key, rating_rule_version_id, mode,
		flat_amount_cents, graduated_tiers, package_size, package_amount_cents,
		overage_unit_amount_cents, COALESCE(reason,''), active, created_at, updated_at
		FROM customer_price_overrides WHERE active = true`

	args := []any{}
	if customerID != "" {
		query += fmt.Sprintf(" AND customer_id = $%d", len(args)+1)
		args = append(args, customerID)
	}
	query += " ORDER BY created_at DESC LIMIT 500"

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var overrides []domain.CustomerPriceOverride
	for rows.Next() {
		var o domain.CustomerPriceOverride
		var tiersJSON []byte
		if err := rows.Scan(&o.ID, &o.TenantID, &o.CustomerID, &o.RuleKey, &o.RatingRuleVersionID,
			&o.Mode, &o.FlatAmountCents, &tiersJSON, &o.PackageSize, &o.PackageAmountCents,
			&o.OverageUnitAmountCents, &o.Reason, &o.Active, &o.CreatedAt, &o.UpdatedAt); err != nil {
			return nil, err
		}
		if len(tiersJSON) > 0 {
			if err := json.Unmarshal(tiersJSON, &o.GraduatedTiers); err != nil {
				return nil, fmt.Errorf("unmarshal graduated_tiers: %w", err)
			}
		}
		overrides = append(overrides, o)
	}
	return overrides, rows.Err()
}

// Override handler methods on the pricing service.

type CreateOverrideInput struct {
	CustomerID string `json:"customer_id"`
	// RatingRuleVersionID identifies WHICH rule to override; it is
	// resolved to the rule's rule_key at this boundary — the key is the
	// override's identity (ADR-070), so the override survives version
	// publishes regardless of which version the caller referenced.
	RatingRuleVersionID    string              `json:"rating_rule_version_id"`
	Mode                   domain.PricingMode  `json:"mode"`
	FlatAmountCents        decimal.Decimal     `json:"flat_amount_cents"`
	GraduatedTiers         []domain.RatingTier `json:"graduated_tiers"`
	PackageSize            int64               `json:"package_size"`
	PackageAmountCents     int64               `json:"package_amount_cents"`
	OverageUnitAmountCents decimal.Decimal     `json:"overage_unit_amount_cents"`
	Reason                 string              `json:"reason,omitempty"`
}

func (s *Service) CreateOverride(ctx context.Context, tenantID string, input CreateOverrideInput) (domain.CustomerPriceOverride, error) {
	if strings.TrimSpace(input.CustomerID) == "" {
		return domain.CustomerPriceOverride{}, errs.Required("customer_id")
	}
	if strings.TrimSpace(input.RatingRuleVersionID) == "" {
		return domain.CustomerPriceOverride{}, errs.Required("rating_rule_version_id")
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

	// Resolve the referenced rule IN-TENANT (RLS scopes the read): a
	// typo'd or cross-tenant id is a clean 422 here — previously the
	// only check was the global FK, which produced a raw constraint
	// error and doubled as a cross-tenant id-existence oracle. The
	// resolved rule also supplies the rule_key the override is keyed by.
	rule, err := s.store.GetRatingRule(ctx, tenantID, input.RatingRuleVersionID)
	if err != nil {
		return domain.CustomerPriceOverride{}, errs.Invalid("rating_rule_version_id",
			fmt.Sprintf("rating rule %q not found", input.RatingRuleVersionID))
	}

	// Full shape validation — same contract as rule authoring. The old
	// qty=1 compute probe never reached tier 2+, so a non-monotonic or
	// bounded-final tier table was accepted here and then hard-failed
	// billOnePeriod at cycle close.
	if err := validatePricingShape(input.Mode, input.FlatAmountCents, input.GraduatedTiers, input.PackageSize, input.PackageAmountCents); err != nil {
		return domain.CustomerPriceOverride{}, err
	}
	// Compute probe against the PATCHED rule — validates the override
	// exactly as the rating paths will apply it.
	o := domain.CustomerPriceOverride{
		CustomerID:             input.CustomerID,
		RuleKey:                rule.RuleKey,
		RatingRuleVersionID:    input.RatingRuleVersionID,
		Mode:                   input.Mode,
		FlatAmountCents:        input.FlatAmountCents,
		GraduatedTiers:         input.GraduatedTiers,
		PackageSize:            input.PackageSize,
		PackageAmountCents:     input.PackageAmountCents,
		OverageUnitAmountCents: input.OverageUnitAmountCents,
		Reason:                 strings.TrimSpace(input.Reason),
	}
	if _, err := domain.ComputeAmountCents(o.ApplyTo(rule), decimal.NewFromInt(1)); err != nil {
		return domain.CustomerPriceOverride{}, errs.Invalid("pricing", fmt.Sprintf("invalid pricing configuration: %v", err))
	}

	return s.store.CreateOverride(ctx, tenantID, o)
}

// DeleteOverride soft-deactivates a negotiated price (ADR-070): list
// price resolves from the next period open; the period in flight keeps
// the price it opened with. The row is kept for audit.
func (s *Service) DeleteOverride(ctx context.Context, tenantID, id string) error {
	if strings.TrimSpace(id) == "" {
		return errs.Required("id")
	}
	return s.store.DeactivateOverride(ctx, tenantID, id)
}

// GetOverrideByKeyAsOf resolves the override in force at asOf — the
// rating paths' lookup (engine close/cancel, threshold, preview).
func (s *Service) GetOverrideByKeyAsOf(ctx context.Context, tenantID, customerID, ruleKey string, asOf time.Time) (domain.CustomerPriceOverride, error) {
	return s.store.GetOverrideByKeyAsOf(ctx, tenantID, customerID, ruleKey, asOf)
}

func (s *Service) ListOverrides(ctx context.Context, tenantID, customerID string) ([]domain.CustomerPriceOverride, error) {
	return s.store.ListOverrides(ctx, tenantID, customerID)
}
