package pricing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// Plan prices (ADR-130). A price is a row on a plan: a meter, the events it
// takes (dimension_match) and its rate. Every write goes through the store's
// one chokepoint (PostgresStore.WritePrice), which decides whether it applies
// at once (a plan no subscription has used) or from each subscriber's next
// period.

// PriceInput is one price to add: its identity (meter and dimension_match)
// and its shape.
type PriceInput struct {
	MeterID        string            `json:"meter_id"`
	DimensionMatch map[string]string `json:"dimension_match"`
	domain.PriceShape
}

// priceAdds checks each input and returns the adds.
func priceAdds(in []PriceInput) ([]PriceChange, error) {
	out := make([]PriceChange, 0, len(in))
	for i, p := range in {
		ch, err := priceAdd(fmt.Sprintf("prices[%d].", i), p)
		if err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	return out, nil
}

// priceAdd checks one price to add. prefix names it in a field error.
func priceAdd(prefix string, p PriceInput) (PriceChange, error) {
	meterID := strings.TrimSpace(p.MeterID)
	if meterID == "" {
		return PriceChange{}, errs.Required(prefix + "meter_id")
	}
	match, err := checkMatch(prefix+"dimension_match", p.DimensionMatch)
	if err != nil {
		return PriceChange{}, err
	}
	shape, err := checkShape(prefix, p.PriceShape)
	if err != nil {
		return PriceChange{}, err
	}
	return PriceChange{Op: PriceAdd, MeterID: meterID, Match: match, Shape: shape}, nil
}

// checkMatch checks a dimension_match against ADR-130 §1: at most 16 labels,
// each key and value a string of 1 to 100 characters. Values are strings
// only: a number would compare one way in Go and another in Postgres.
func checkMatch(field string, m map[string]string) (map[string]string, error) {
	if len(m) > domain.MaxDimensionKeys {
		return nil, errs.Invalid(field, fmt.Sprintf("at most %d labels (got %d)", domain.MaxDimensionKeys, len(m)))
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		switch {
		case strings.TrimSpace(k) == "":
			return nil, errs.Invalid(field, "a label name is empty")
		case len(k) > domain.MaxDimensionKeyLength:
			return nil, errs.Invalid(field, fmt.Sprintf("label %q is longer than %d characters", k[:20]+"…", domain.MaxDimensionKeyLength))
		case v == "":
			return nil, errs.Invalid(field, fmt.Sprintf("label %q has an empty value", k))
		case len(v) > domain.MaxDimensionKeyLength:
			return nil, errs.Invalid(field, fmt.Sprintf("the value of label %q is longer than %d characters", k, domain.MaxDimensionKeyLength))
		}
		out[k] = v
	}
	return out, nil
}

// checkShape checks a price's or an override's amounts and shape and returns
// the shape to store. prefix names the price in a field error.
func checkShape(prefix string, s domain.PriceShape) (domain.PriceShape, error) {
	unit, tiers, pkg, err := checkRates(s.UnitAmountCents, s.Tiers, s.PackageAmountCents)
	if err != nil {
		return domain.PriceShape{}, prefixField(prefix, err)
	}
	s.UnitAmountCents, s.Tiers, s.PackageAmountCents = unit, tiers, pkg
	if err := validatePricingShape(s.Mode, s.UnitAmountCents, s.Tiers, s.PackageSize, s.PackageAmountCents); err != nil {
		return domain.PriceShape{}, prefixField(prefix, err)
	}
	return s, nil
}

func prefixField(prefix string, err error) error {
	var de *errs.DomainError
	if prefix != "" && errors.As(err, &de) && de.Field != "" {
		de.Field = prefix + de.Field
	}
	return err
}

// checkRates checks every amount of a price or a price override against
// domain.CheckRate and returns the amounts to store. It runs in every mode,
// on the amounts the mode does not use too (validatePricingShape then
// refuses any of those that is not 0), and before anything prices them, so
// no arithmetic sees an amount outside the limits.
func checkRates(unit decimal.Decimal, tiers []domain.RatingTier, pkg decimal.Decimal) (decimal.Decimal, []domain.RatingTier, decimal.Decimal, error) {
	var err error
	if unit, err = domain.CheckRate("unit_amount_cents", unit); err != nil {
		return decimal.Decimal{}, nil, decimal.Decimal{}, err
	}
	if tiers != nil {
		checked := make([]domain.RatingTier, len(tiers))
		for i, t := range tiers {
			if t.UnitAmountCents, err = domain.CheckRate(fmt.Sprintf("tiers[%d].unit_amount_cents", i), t.UnitAmountCents); err != nil {
				return decimal.Decimal{}, nil, decimal.Decimal{}, err
			}
			checked[i] = t
		}
		tiers = checked
	}
	if pkg, err = domain.CheckRate("package_amount_cents", pkg); err != nil {
		return decimal.Decimal{}, nil, decimal.Decimal{}, err
	}
	return unit, tiers, pkg, nil
}

// validatePricingShape enforces the shape of a price at authoring time,
// shared by prices and customer overrides. It is domain.CheckPriceShape: the
// mode's fields and only those (a field the mode does not use must be zero,
// tiers empty). The tier rules are domain.CheckTiers, the same check
// ComputeAmountCents runs, so a table authoring accepts always rates.
//
// Zero is a legal amount: "this dimension is free", and a zero-price tier
// expresses an included allowance, "first 1M tokens free, then $2/M".
func validatePricingShape(mode domain.PricingMode, unit decimal.Decimal, tiers []domain.RatingTier, pkgSize int64, pkg decimal.Decimal) error {
	if err := domain.CheckPriceShape(mode, unit, tiers, pkgSize, pkg); err != nil {
		var fe *domain.PriceFieldError
		if errors.As(err, &fe) {
			return errs.Invalid(fe.Field, fe.Reason)
		}
		return err
	}
	return nil
}

// AddPrice adds a price to the plan. On a plan no subscription has used it
// applies at once; on a used plan, from each subscriber's next period. A
// price that names a removed price's meter and dimension_match adds that
// price back under its id.
func (s *Service) AddPrice(ctx context.Context, tenantID, planID string, in PriceInput) (PriceWrite, error) {
	ch, err := priceAdd("", in)
	if err != nil {
		return PriceWrite{}, err
	}
	return s.store.WritePrice(ctx, tenantID, planID, ch, s.priceEmission(ctx, planID))
}

// EditPrice changes a price's content. Its meter and dimension_match are its
// identity: to price other events, add a price.
func (s *Service) EditPrice(ctx context.Context, tenantID, planID, priceID string, shape domain.PriceShape) (PriceWrite, error) {
	checked, err := checkShape("", shape)
	if err != nil {
		return PriceWrite{}, err
	}
	return s.store.WritePrice(ctx, tenantID, planID, PriceChange{Op: PriceEdit, PriceID: priceID, Shape: checked}, s.priceEmission(ctx, planID))
}

// RemovePrice removes a price from the plan and ends its customer overrides
// at the instant the removal takes effect for each customer (ADR-130 §6).
func (s *Service) RemovePrice(ctx context.Context, tenantID, planID, priceID string) (PriceWrite, error) {
	return s.store.WritePrice(ctx, tenantID, planID, PriceChange{Op: PriceRemove, PriceID: priceID}, s.priceEmission(ctx, planID))
}

// ListPlanPrices returns the plan's prices from now on. 404 for an unknown
// plan, so a typo'd id does not read as a plan with no prices.
func (s *Service) ListPlanPrices(ctx context.Context, tenantID, planID string) ([]domain.PlanPrice, error) {
	if _, err := s.store.GetPlan(ctx, tenantID, planID); err != nil {
		return nil, notFoundAs(err, "plan")
	}
	byPlan, err := s.store.ListPrices(ctx, tenantID, []string{planID})
	if err != nil {
		return nil, err
	}
	return byPlan[planID], nil
}

// ListPrices returns the prices of the plans from now on, by plan id.
func (s *Service) ListPrices(ctx context.Context, tenantID string, planIDs []string) (map[string][]domain.PlanPrice, error) {
	return s.store.ListPrices(ctx, tenantID, planIDs)
}

// PlanPricesAt returns the plan's prices taking part at `at` in the time
// domain of clockID ("" is wall time), in claim order per meter: the one
// price resolution every reader of a billing period shares (ADR-130 §4).
func (s *Service) PlanPricesAt(ctx context.Context, tenantID, planID string, at time.Time, clockID string) ([]domain.PlanPrice, error) {
	return s.store.PlanPricesAt(ctx, tenantID, planID, at, clockID)
}

// PlanMeterIDs returns every meter the plan has ever priced: the set the
// overlap guard reads.
func (s *Service) PlanMeterIDs(ctx context.Context, tenantID, planID string) ([]string, error) {
	return s.store.PlanMeterIDs(ctx, tenantID, planID)
}

// priceEmission builds the in-tx audit row of a price write (ADR-090): the
// content before and after, when it applies, and the overrides a removal
// ended. A never-used plan's write replaces the earlier content, so the audit
// row is the only record of it.
func (s *Service) priceEmission(ctx context.Context, planID string) func(tx *sql.Tx, r PriceWrite) error {
	if s.auditLogger == nil {
		return nil
	}
	return func(tx *sql.Tx, r PriceWrite) error {
		action := domain.AuditActionUpdate
		switch {
		case r.Removed:
			action = domain.AuditActionDelete
		case r.Before == nil:
			action = domain.AuditActionCreate
		}
		meta := map[string]any{
			"plan_id":         planID,
			"meter_id":        r.Price.MeterID,
			"dimension_match": r.Price.DimensionMatch,
			"effective":       string(r.Effective),
		}
		if r.Before != nil {
			meta["before"] = r.Before
		}
		if !r.Removed {
			meta["after"] = r.Price.PriceShape
		}
		if r.ReAdded {
			meta["added_back"] = true
		}
		if len(r.EndedOverrides) > 0 {
			meta["ended_overrides"] = r.EndedOverrides
		}
		return s.auditLogger.LogInTx(ctx, tx, audit.Entry{
			Action:       action,
			ResourceType: "plan_price",
			ResourceID:   r.Price.ID,
			Metadata:     meta,
		})
	}
}
