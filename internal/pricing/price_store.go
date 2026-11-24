package pricing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// The plan price store (ADR-130 §1, §4-§8).
//
// A price is a plan_prices row, its identity, and a list of versions, its
// content over time. Which version prices a billing period depends on
// whether the plan has been used.
//
//   - On a plan no subscription has used, a write applies at once: the price
//     keeps one version, from the beginning of time ('-infinity'), and the
//     write replaces every earlier version. Every other price of the plan
//     keeps only its latest version, from the beginning of time too
//     (settleNeverUsedTx).
//   - On a used plan, a write appends a version that takes effect when it
//     commits: valid_from is clock_timestamp() read after the plan lock, and
//     at least 1 microsecond after the plan's latest valid_from, so versions
//     on a plan take effect in the order their writes commit.
//
// A plan is priced at its pin of the period: the period start, or the first
// instant in the period the subscription joined the plan (ADR-130 §4 step
// 3). A version is in force at the pin when valid_from <= pin, and the
// latest such version wins. A price whose winning version is a removal takes
// no part.

// versionInForce is the price version in force at $2, from the version list
// of p.
var versionInForce = versionInForceAt("$2")

// versionInForceAt is versionInForce with the instant given as an SQL
// expression.
func versionInForceAt(at string) string {
	return `
	SELECT v.id, v.removed, v.mode, v.unit_amount_cents, v.tiers, v.package_size, v.package_amount_cents, v.created_at
	FROM plan_price_versions v
	WHERE v.price_id = p.id
	  AND v.valid_from <= ` + at + `
	ORDER BY v.valid_from DESC
	LIMIT 1`
}

// latestVersion is a price's latest version: its content from now on.
const latestVersion = `
	SELECT v.id, v.removed, v.mode, v.unit_amount_cents, v.tiers, v.package_size, v.package_amount_cents, v.created_at
	FROM plan_price_versions v
	WHERE v.price_id = p.id
	ORDER BY v.valid_from DESC
	LIMIT 1`

const priceIdentityColumns = `p.id, p.tenant_id, p.plan_id, p.meter_id, p.dimension_match, p.created_at`

const versionColumns = `v.id, v.removed, v.mode, v.unit_amount_cents, v.tiers, v.package_size, v.package_amount_cents, v.created_at`

// priceRow is a price with one of its versions.
type priceRow struct {
	price     domain.PlanPrice
	versionID string
	removed   bool
}

func scanPriceRow(row rowScanner) (priceRow, error) {
	var (
		r         priceRow
		match     []byte
		versionID sql.NullString
		removed   sql.NullBool
		mode      sql.NullString
		unit      decimal.NullDecimal
		tiers     []byte
		pkgSize   sql.NullInt64
		pkgAmount decimal.NullDecimal
		updated   sql.NullTime
	)
	err := row.Scan(&r.price.ID, &r.price.TenantID, &r.price.PlanID, &r.price.MeterID, &match, &r.price.CreatedAt,
		&versionID, &removed, &mode, &unit, &tiers, &pkgSize, &pkgAmount, &updated)
	if err == sql.ErrNoRows {
		return priceRow{}, errs.ErrNotFound
	}
	if err != nil {
		return priceRow{}, err
	}
	if err := json.Unmarshal(match, &r.price.DimensionMatch); err != nil {
		return priceRow{}, fmt.Errorf("decode dimension_match of price %s: %w", r.price.ID, err)
	}
	if r.price.DimensionMatch == nil {
		r.price.DimensionMatch = map[string]string{}
	}
	// A price is written with its first version on one transaction, so a
	// price with none is not one any reader may bill.
	r.versionID = versionID.String
	r.removed = !versionID.Valid || removed.Bool
	r.price.UpdatedAt = updated.Time
	if r.removed {
		return r, nil
	}
	r.price.Mode = domain.PricingMode(mode.String)
	r.price.UnitAmountCents = unit.Decimal
	r.price.PackageSize = pkgSize.Int64
	r.price.PackageAmountCents = pkgAmount.Decimal
	if len(tiers) > 0 {
		if err := json.Unmarshal(tiers, &r.price.Tiers); err != nil {
			return priceRow{}, fmt.Errorf("decode tiers of price %s: %w", r.price.ID, err)
		}
	}
	return r, nil
}

func scanPriceRows(rows *sql.Rows) ([]priceRow, error) {
	defer func() { _ = rows.Close() }()
	var out []priceRow
	for rows.Next() {
		r, err := scanPriceRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PlanPricesAt returns the plan's prices taking part at `at`, in claim order
// per meter.
func (s *PostgresStore) PlanPricesAt(ctx context.Context, tenantID, planID string, at time.Time) ([]domain.PlanPrice, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(ctx, `
		SELECT `+priceIdentityColumns+`, `+versionColumns+`
		FROM plan_prices p
		CROSS JOIN LATERAL (`+versionInForce+`) v
		WHERE p.plan_id = $1 AND NOT v.removed`, planID, at)
	if err != nil {
		return nil, fmt.Errorf("read prices of plan %s: %w", planID, err)
	}
	found, err := scanPriceRows(rows)
	if err != nil {
		return nil, fmt.Errorf("read prices of plan %s: %w", planID, err)
	}
	out := make([]domain.PlanPrice, 0, len(found))
	for _, r := range found {
		out = append(out, r.price)
	}
	domain.ClaimOrder(out)
	return out, nil
}

// PlanPin is one read of a plan's prices, at At.
type PlanPin struct {
	PlanID string
	At     time.Time
}

// PriceMatch is one price as usage ingest reads it: its meter and the labels
// it takes (dimension_match; {} takes every event of the meter).
type PriceMatch struct {
	MeterID string
	Match   map[string]string
}

// PinPrices is one pin's answer to PriceMatchesAt.
type PinPrices struct {
	// Matches is the meter and dimension_match of every price the plan has
	// taking part at the pin (the prices of PlanPricesAt).
	Matches []PriceMatch
	// Passed: the pin had passed when the prices were read, so a price saved
	// after the read is not in force at it (a used plan's version takes
	// effect from its write, ADR-130 §7). At a pin still ahead (the next
	// period's start, a trial end) a price saved now is.
	Passed bool
}

// PriceMatchesAt returns, for each pin, the meter and dimension_match of
// every price its plan has taking part then (the prices of PlanPricesAt),
// and whether the pin had passed, in one transaction. Ingest reads every
// open period's pins once per customer per request, before it writes, and
// matches each event's labels against them in Go (usage.takes). A pin has
// passed when it is before the transaction's start.
func (s *PostgresStore) PriceMatchesAt(ctx context.Context, tenantID string, pins []PlanPin) ([]PinPrices, error) {
	out := make([]PinPrices, len(pins))
	if len(pins) == 0 {
		return out, nil
	}
	planIDs := make([]string, len(pins))
	ats := make([]string, len(pins))
	for i, p := range pins {
		planIDs[i] = p.PlanID
		// Microseconds, truncated: what a timestamptz parameter carries.
		ats[i] = p.At.UTC().Format("2006-01-02T15:04:05.999999Z07:00")
	}
	args := []any{planIDs, ats}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)
	passed, err := tx.QueryContext(ctx, `
		SELECT r.ord, r.at < now()
		FROM unnest($1::text[], $2::text[]::timestamptz[]) WITH ORDINALITY AS r(plan_id, at, ord)`, args...)
	if err != nil {
		return nil, fmt.Errorf("read whether the pins have passed: %w", err)
	}
	for passed.Next() {
		var ord int
		var ok bool
		if err := passed.Scan(&ord, &ok); err != nil {
			_ = passed.Close()
			return nil, err
		}
		out[ord-1].Passed = ok
	}
	if err := passed.Close(); err != nil {
		return nil, err
	}
	if err := passed.Err(); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT r.ord, p.meter_id, p.dimension_match
		FROM unnest($1::text[], $2::text[]::timestamptz[]) WITH ORDINALITY AS r(plan_id, at, ord)
		JOIN plan_prices p ON p.plan_id = r.plan_id
		CROSS JOIN LATERAL (`+versionInForceAt("r.at")+`) v
		WHERE NOT v.removed
		ORDER BY r.ord, p.meter_id, p.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("read the prices of plans: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var ord int
		var pm PriceMatch
		var match []byte
		if err := rows.Scan(&ord, &pm.MeterID, &match); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(match, &pm.Match); err != nil {
			return nil, fmt.Errorf("decode dimension_match: %w", err)
		}
		if pm.Match == nil {
			pm.Match = map[string]string{}
		}
		out[ord-1].Matches = append(out[ord-1].Matches, pm)
	}
	return out, rows.Err()
}

// ListPrices returns the prices of the plans taking part from now on.
func (s *PostgresStore) ListPrices(ctx context.Context, tenantID string, planIDs []string) (map[string][]domain.PlanPrice, error) {
	out := make(map[string][]domain.PlanPrice, len(planIDs))
	if len(planIDs) == 0 {
		return out, nil
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(ctx, `
		SELECT `+priceIdentityColumns+`, `+versionColumns+`
		FROM plan_prices p
		CROSS JOIN LATERAL (`+latestVersion+`) v
		WHERE p.plan_id = ANY($1::text[]) AND NOT v.removed
		ORDER BY p.plan_id, p.created_at, p.id`, planIDs)
	if err != nil {
		return nil, fmt.Errorf("list plan prices: %w", err)
	}
	found, err := scanPriceRows(rows)
	if err != nil {
		return nil, fmt.Errorf("list plan prices: %w", err)
	}
	for _, r := range found {
		out[r.price.PlanID] = append(out[r.price.PlanID], r.price)
	}
	return out, nil
}

// PlanMeterIDs returns every meter the plan has ever priced.
func (s *PostgresStore) PlanMeterIDs(ctx context.Context, tenantID, planID string) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT meter_id FROM plan_prices WHERE plan_id = $1 ORDER BY meter_id`, planID)
	if err != nil {
		return nil, fmt.Errorf("read meters of plan %s: %w", planID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// latestPricesOnMeterTx is every price row of the plan and meter, removed
// ones included, each with its latest version.
func latestPricesOnMeterTx(ctx context.Context, tx *sql.Tx, planID, meterID string) ([]priceRow, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT `+priceIdentityColumns+`, `+versionColumns+`
		FROM plan_prices p
		LEFT JOIN LATERAL (`+latestVersion+`) v ON true
		WHERE p.plan_id = $1 AND p.meter_id = $2
		ORDER BY p.created_at, p.id`, planID, meterID)
	if err != nil {
		return nil, fmt.Errorf("read prices of plan %s on meter %s: %w", planID, meterID, err)
	}
	return scanPriceRows(rows)
}

// latestPriceTx is one price of the plan with its latest version.
func latestPriceTx(ctx context.Context, tx *sql.Tx, planID, priceID string) (priceRow, error) {
	return scanPriceRow(tx.QueryRowContext(ctx, `
		SELECT `+priceIdentityColumns+`, `+versionColumns+`
		FROM plan_prices p
		LEFT JOIN LATERAL (`+latestVersion+`) v ON true
		WHERE p.id = $1 AND p.plan_id = $2`, priceID, planID))
}

// WritePrice applies ch to the plan's prices. See the top of this file.
func (s *PostgresStore) WritePrice(ctx context.Context, tenantID, planID string, ch PriceChange, emit func(tx *sql.Tx, r PriceWrite) error) (PriceWrite, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return PriceWrite{}, err
	}
	defer postgres.Rollback(tx)

	// Lock order (ADR-130 §8): the plan, FOR UPDATE as a writer of plan
	// content; the meter, FOR KEY SHARE through the new price's foreign key;
	// then the override rows a removal ends.
	if _, err := lockPlanTx(ctx, tx, planID); err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return PriceWrite{}, errs.NotFound("plan")
		}
		return PriceWrite{}, err
	}
	used, err := planUsedTx(ctx, tx, planID)
	if err != nil {
		return PriceWrite{}, err
	}

	var (
		target priceRow
		out    = PriceWrite{Effective: EffectiveNow, Removed: ch.Op == PriceRemove}
	)
	if used {
		out.Effective = EffectiveNextPeriod
	}
	switch ch.Op {
	case PriceAdd:
		target, out.ReAdded, err = s.addIdentityTx(ctx, tx, tenantID, planID, used, ch)
		if err != nil {
			return PriceWrite{}, err
		}
	case PriceEdit, PriceRemove:
		target, err = latestPriceTx(ctx, tx, planID, ch.PriceID)
		if errors.Is(err, errs.ErrNotFound) || (err == nil && target.removed) {
			return PriceWrite{}, errs.NotFound("price")
		}
		if err != nil {
			return PriceWrite{}, err
		}
		before := target.price.PriceShape
		out.Before = &before
	default:
		return PriceWrite{}, fmt.Errorf("unknown price change %d", ch.Op)
	}

	shape := ch.Shape
	if ch.Op == PriceRemove {
		shape = target.price.PriceShape
	}
	if !used {
		if err := settleNeverUsedTx(ctx, tx, planID); err != nil {
			return PriceWrite{}, err
		}
	}
	v, err := writeVersionTx(ctx, tx, tenantID, planID, target.price.ID, used, ch.Op == PriceRemove, ch.Shape)
	if err != nil {
		return PriceWrite{}, err
	}
	out.Price = target.price
	out.Price.PriceShape = shape
	out.Price.UpdatedAt = v.createdAt
	if ch.Op == PriceRemove {
		out.EndedOverrides, err = endOverridesTx(ctx, tx, target.price.ID, used, v)
		if err != nil {
			return PriceWrite{}, err
		}
	}
	if emit != nil {
		if err := emit(tx, out); err != nil {
			return PriceWrite{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return PriceWrite{}, err
	}
	return out, nil
}

// addIdentityTx finds or makes the price row an add writes to, after the
// checks of ADR-130 §4: an identical dimension_match on the plan and meter is
// price_exists while that price takes part, and is added back (the same row,
// the same id) when it was removed; a price that ties with one that takes
// part is price_conflict; a meter new to a used plan must not overlap a
// subscriber's other subscription (meter_overlap).
func (s *PostgresStore) addIdentityTx(ctx context.Context, tx *sql.Tx, tenantID, planID string, used bool, ch PriceChange) (priceRow, bool, error) {
	// The meter must be the caller's: a foreign key check does not apply
	// row-level security, so the insert alone would accept another tenant's
	// or the other mode's meter.
	if _, err := s.GetMeterTx(ctx, tx, ch.MeterID); err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return priceRow{}, false, errs.Invalid("meter_id", fmt.Sprintf("meter %q not found", ch.MeterID))
		}
		return priceRow{}, false, err
	}
	rows, err := latestPricesOnMeterTx(ctx, tx, planID, ch.MeterID)
	if err != nil {
		return priceRow{}, false, err
	}
	if err := checkAdd(rows, ch.Match); err != nil {
		return priceRow{}, false, err
	}
	for _, r := range rows {
		if domain.SameMatch(r.price.DimensionMatch, ch.Match) {
			return r, true, nil // a removed price, added back
		}
	}
	if used && len(rows) == 0 {
		if err := refuseMeterOverlapTx(ctx, tx, planID, ch.MeterID); err != nil {
			return priceRow{}, false, err
		}
	}
	p, err := insertPriceTx(ctx, tx, tenantID, planID, ch.MeterID, ch.Match)
	if err != nil {
		return priceRow{}, false, err
	}
	return priceRow{price: p}, false, nil
}

// checkAdd checks a new match against the plan's prices on its meter (rows,
// each with its latest version). Only prices that take part from now on
// count: a removed price and a later price tying with it are never in force
// at one pin, because versions on a plan take effect in the order their
// writes commit (ADR-130 §4).
func checkAdd(rows []priceRow, match map[string]string) error {
	for _, r := range rows {
		if r.removed {
			continue
		}
		if domain.SameMatch(r.price.DimensionMatch, match) {
			return errs.AlreadyExists("dimension_match", fmt.Sprintf(
				"Price %s already prices this meter with the same dimension_match. Edit that price instead.", r.price.ID)).WithCode(errs.CodePriceExists)
		}
		if domain.PricesTie(r.price.DimensionMatch, match) {
			return errs.AlreadyExists("dimension_match", fmt.Sprintf(
				"Price %s could take the same events with the same number of labels, so neither would win. "+
					"Add a label to make one of them more specific, or give a shared label a different value.", r.price.ID)).WithCode(errs.CodePriceConflict)
		}
	}
	return nil
}

// insertPriceTx writes a price's identity. Its created_at is the database's
// wall clock at the insert, not the transaction's start, so the prices of
// one POST /v1/plans keep the order they were sent in (domain.ClaimOrder).
func insertPriceTx(ctx context.Context, tx *sql.Tx, tenantID, planID, meterID string, match map[string]string) (domain.PlanPrice, error) {
	if match == nil {
		match = map[string]string{}
	}
	raw, err := json.Marshal(match)
	if err != nil {
		return domain.PlanPrice{}, fmt.Errorf("encode dimension_match: %w", err)
	}
	p := domain.PlanPrice{TenantID: tenantID, PlanID: planID, MeterID: meterID, DimensionMatch: match}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO plan_prices (id, tenant_id, plan_id, meter_id, dimension_match, created_at)
		VALUES ($1, $2, $3, $4, $5, clock_timestamp())
		RETURNING id, created_at`,
		postgres.NewID("hc_prc"), tenantID, planID, meterID, raw).Scan(&p.ID, &p.CreatedAt)
	if err != nil {
		if postgres.IsUniqueViolation(err) {
			return domain.PlanPrice{}, errs.AlreadyExists("dimension_match", "This plan already prices this meter with the same dimension_match.").WithCode(errs.CodePriceExists)
		}
		return domain.PlanPrice{}, fmt.Errorf("insert price: %w", err)
	}
	return p, nil
}

// settleNeverUsedTx puts every price of a never-used plan at its latest
// version from the beginning of time (ADR-130 §5). A plan whose only
// references were scheduled plan changes onto it is never-used again once
// they are cleared or replaced before they apply, and then it can hold
// versions written while it was used. A write to a never-used plan applies
// at once, so no older version may stay in force at an earlier pin: a price
// removed while the plan was used would otherwise still take events at those
// pins, beside a price added now that ties with it. No period of the plan
// has been priced, so nothing billed read the versions it drops.
func settleNeverUsedTx(ctx context.Context, tx *sql.Tx, planID string) error {
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM plan_price_versions v
		USING plan_prices p
		WHERE p.id = v.price_id AND p.plan_id = $1
		  AND v.id <> (SELECT l.id FROM plan_price_versions l WHERE l.price_id = v.price_id ORDER BY l.valid_from DESC LIMIT 1)`,
		planID); err != nil {
		return fmt.Errorf("drop the older price versions of plan %s: %w", planID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE plan_price_versions v SET valid_from = '-infinity'
		FROM plan_prices p
		WHERE p.id = v.price_id AND p.plan_id = $1 AND v.valid_from <> '-infinity'`,
		planID); err != nil {
		return fmt.Errorf("put the prices of plan %s from the beginning of time: %w", planID, err)
	}
	return nil
}

// writtenVersion is the version a write appended or put in place.
type writtenVersion struct {
	id        string
	validFrom time.Time
	infinite  bool // valid_from is '-infinity': the plan was never used
	createdAt time.Time
}

// writeVersionTx writes the price's next version. On a plan never used it
// replaces the price's versions with one from the beginning of time; on a
// used plan it appends one from now.
func writeVersionTx(ctx context.Context, tx *sql.Tx, tenantID, planID, priceID string, used, removed bool, shape domain.PriceShape) (writtenVersion, error) {
	v := writtenVersion{id: postgres.NewID("hc_ppv")}
	if !used {
		if _, err := tx.ExecContext(ctx, `DELETE FROM plan_price_versions WHERE price_id = $1`, priceID); err != nil {
			return writtenVersion{}, fmt.Errorf("replace versions of price %s: %w", priceID, err)
		}
		v.infinite = true
	} else {
		// clock_timestamp(), not now(): now() is the transaction's start, so
		// of two racing writes the one saved last could get the earlier time
		// and never apply. Read after the plan lock, and at least 1
		// microsecond after the plan's latest version, so versions on a plan
		// take effect in the order their writes commit (ADR-130 §7).
		if err := tx.QueryRowContext(ctx, `
			SELECT GREATEST(clock_timestamp(), COALESCE(MAX(v.valid_from), '-infinity') + interval '1 microsecond')
			FROM plan_price_versions v JOIN plan_prices p ON p.id = v.price_id
			WHERE p.plan_id = $1`, planID).Scan(&v.validFrom); err != nil {
			return writtenVersion{}, fmt.Errorf("read the time of plan %s's next price version: %w", planID, err)
		}
	}

	var (
		mode      sql.NullString
		unit      decimal.NullDecimal
		tiers     []byte
		pkgSize   sql.NullInt64
		pkgAmount decimal.NullDecimal
	)
	if !removed {
		mode = sql.NullString{String: string(shape.Mode), Valid: true}
		switch shape.Mode {
		case domain.PricingPerUnit:
			unit = decimal.NullDecimal{Decimal: shape.UnitAmountCents, Valid: true}
		case domain.PricingGraduated:
			raw, err := json.Marshal([]domain.RatingTier(shape.Tiers))
			if err != nil {
				return writtenVersion{}, fmt.Errorf("encode tiers: %w", err)
			}
			tiers = raw
		case domain.PricingPackage:
			pkgSize = sql.NullInt64{Int64: shape.PackageSize, Valid: true}
			pkgAmount = decimal.NullDecimal{Decimal: shape.PackageAmountCents, Valid: true}
		}
	}
	validFrom := any(v.validFrom)
	if v.infinite {
		validFrom = "-infinity"
	}
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO plan_price_versions (id, tenant_id, price_id, valid_from, removed,
			mode, unit_amount_cents, tiers, package_size, package_amount_cents)
		VALUES ($1, $2, $3, $4::timestamptz, $5, $6, $7, $8, $9, $10)
		RETURNING created_at`,
		v.id, tenantID, priceID, validFrom, removed, mode, unit, tiers, pkgSize, pkgAmount).Scan(&v.createdAt); err != nil {
		return writtenVersion{}, fmt.Errorf("write a version of price %s: %w", priceID, err)
	}
	return v, nil
}

// endOverridesTx ends every open override of a removed price (ADR-130 §6),
// at the instant the removal takes effect: the tombstone's valid_from. So at
// every pin an override is open exactly while its price takes part, and the
// open period keeps the negotiated rate. On a plan never used the removal
// applies at once, so each window becomes empty.
func endOverridesTx(ctx context.Context, tx *sql.Tx, priceID string, used bool, v writtenVersion) ([]EndedOverride, error) {
	var rows *sql.Rows
	var err error
	if used {
		rows, err = tx.QueryContext(ctx, `
			UPDATE customer_price_overrides o
			SET active = false, updated_at = now(), deactivated_at = GREATEST(o.created_at, $2::timestamptz)
			WHERE o.price_id = $1 AND o.active
			RETURNING o.id, o.customer_id`, priceID, v.validFrom)
	} else {
		rows, err = tx.QueryContext(ctx, `
			UPDATE customer_price_overrides o
			SET active = false, updated_at = now(), deactivated_at = o.created_at
			WHERE o.price_id = $1 AND o.active
			RETURNING o.id, o.customer_id`, priceID)
	}
	if err != nil {
		return nil, fmt.Errorf("end the overrides of price %s: %w", priceID, err)
	}
	defer func() { _ = rows.Close() }()
	out := []EndedOverride{}
	for rows.Next() {
		var e EndedOverride
		if err := rows.Scan(&e.ID, &e.CustomerID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// refuseMeterOverlapTx refuses a price that would add meterID to a used plan
// when a customer with a live subscription on the plan already has the
// meter on another item: two live items on one meter would each bill the
// same usage (rejectMeterOverlap in the subscription service is the same
// rule at subscription writes). A plan's meters are every meter it ever
// priced. It reads without locks: a subscription created on another plan at
// the same moment is not ordered with it (ADR-130 Risks).
func refuseMeterOverlapTx(ctx context.Context, tx *sql.Tx, planID, meterID string) error {
	var code string
	err := tx.QueryRowContext(ctx, `
		SELECT s2.code
		FROM subscriptions s
		JOIN subscription_items si ON si.subscription_id = s.id AND si.deleted_at IS NULL
		     AND (si.plan_id = $1 OR si.pending_plan_id = $1)
		JOIN subscriptions s2 ON s2.customer_id = s.customer_id AND s2.status IN ('trialing', 'active')
		JOIN subscription_items si2 ON si2.subscription_id = s2.id AND si2.deleted_at IS NULL AND si2.id <> si.id
		JOIN plan_prices pp ON pp.meter_id = $2 AND (pp.plan_id = si2.plan_id OR pp.plan_id = si2.pending_plan_id)
		WHERE s.status IN ('trialing', 'active')
		ORDER BY s2.code
		LIMIT 1`, planID, meterID).Scan(&code)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check the plan's subscribers for meter %s: %w", meterID, err)
	}
	return errs.InvalidState(fmt.Sprintf(
		"A customer on this plan already has this meter on subscription %q, so its usage would be invoiced twice. "+
			"Price the meter on a new plan instead.", code)).WithCode(errs.CodeMeterOverlap)
}

// WritePricesTx adds prices to a plan created on tx. The plan has never
// been used, so each price takes one version from the beginning of time.
// The adds are checked against each other as WritePrice checks one add
// against the plan's prices.
func (s *PostgresStore) WritePricesTx(ctx context.Context, tx *sql.Tx, planID string, adds []PriceChange) ([]domain.PlanPrice, error) {
	var tenantID string
	if err := tx.QueryRowContext(ctx, `SELECT tenant_id FROM plans WHERE id = $1`, planID).Scan(&tenantID); err != nil {
		return nil, fmt.Errorf("read plan %s: %w", planID, err)
	}
	out := make([]domain.PlanPrice, 0, len(adds))
	byMeter := map[string][]priceRow{}
	for i, ch := range adds {
		if _, ok := byMeter[ch.MeterID]; !ok {
			if _, err := s.GetMeterTx(ctx, tx, ch.MeterID); err != nil {
				if errors.Is(err, errs.ErrNotFound) {
					return nil, errs.Invalid(fmt.Sprintf("prices[%d].meter_id", i), fmt.Sprintf("meter %q not found", ch.MeterID))
				}
				return nil, err
			}
		}
		if err := checkAdd(byMeter[ch.MeterID], ch.Match); err != nil {
			var de *errs.DomainError
			if errors.As(err, &de) {
				de.Field = fmt.Sprintf("prices[%d].dimension_match", i)
			}
			return nil, err
		}
		p, err := insertPriceTx(ctx, tx, tenantID, planID, ch.MeterID, ch.Match)
		if err != nil {
			return nil, err
		}
		v, err := writeVersionTx(ctx, tx, tenantID, planID, p.ID, false, false, ch.Shape)
		if err != nil {
			return nil, err
		}
		p.PriceShape = ch.Shape
		p.UpdatedAt = v.createdAt
		byMeter[ch.MeterID] = append(byMeter[ch.MeterID], priceRow{price: p})
		out = append(out, p)
	}
	return out, nil
}
