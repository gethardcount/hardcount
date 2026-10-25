package pricing

import (
	"encoding/json"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// The rating-rule and price-override create bodies keep their rates raw, so
// every bad rate is a 422 naming the field (domain.ParseDecimal) rather than a
// 400 for the whole body, and an exponent too large to read has its own
// message. The limits are the service's (checkRates), so a Go caller such as
// the recipe installer gets the same ones.

// rateTierRequest is one graduated tier on the wire. up_to is a whole number,
// or null for the open last tier.
type rateTierRequest struct {
	UpTo            *int64          `json:"up_to"`
	UnitAmountCents json.RawMessage `json:"unit_amount_cents"`
}

type ratingRuleRequest struct {
	RuleKey            string             `json:"rule_key"`
	Name               string             `json:"name"`
	Mode               domain.PricingMode `json:"mode"`
	Currency           string             `json:"currency"`
	UnitAmountCents    json.RawMessage    `json:"unit_amount_cents"`
	Tiers              []rateTierRequest  `json:"tiers"`
	PackageSize        int64              `json:"package_size"`
	PackageAmountCents json.RawMessage    `json:"package_amount_cents"`
}

func (r ratingRuleRequest) input() (CreateRatingRuleInput, error) {
	unit, tiers, pkg, err := parseRates(r.UnitAmountCents, r.Tiers, r.PackageAmountCents)
	if err != nil {
		return CreateRatingRuleInput{}, err
	}
	return CreateRatingRuleInput{
		RuleKey: r.RuleKey, Name: r.Name, Mode: r.Mode, Currency: r.Currency,
		UnitAmountCents: unit, Tiers: tiers,
		PackageSize: r.PackageSize, PackageAmountCents: pkg,
	}, nil
}

type overrideRequest struct {
	CustomerID          string             `json:"customer_id"`
	RatingRuleVersionID string             `json:"rating_rule_version_id"`
	Mode                domain.PricingMode `json:"mode"`
	UnitAmountCents     json.RawMessage    `json:"unit_amount_cents"`
	Tiers               []rateTierRequest  `json:"tiers"`
	PackageSize         int64              `json:"package_size"`
	PackageAmountCents  json.RawMessage    `json:"package_amount_cents"`
	Reason              string             `json:"reason,omitempty"`
}

func (r overrideRequest) input() (CreateOverrideInput, error) {
	unit, tiers, pkg, err := parseRates(r.UnitAmountCents, r.Tiers, r.PackageAmountCents)
	if err != nil {
		return CreateOverrideInput{}, err
	}
	return CreateOverrideInput{
		CustomerID: r.CustomerID, RatingRuleVersionID: r.RatingRuleVersionID, Mode: r.Mode,
		UnitAmountCents: unit, Tiers: tiers,
		PackageSize: r.PackageSize, PackageAmountCents: pkg, Reason: r.Reason,
	}, nil
}

// parseRates reads the amounts. An absent or null amount is 0, as it was
// when the body decoded straight into decimals: a mode leaves the amounts it
// does not use unset. An absent tiers stays nil.
func parseRates(unitRaw json.RawMessage, tierReqs []rateTierRequest, pkgRaw json.RawMessage) (unit decimal.Decimal, tiers []domain.RatingTier, pkg decimal.Decimal, err error) {
	if unit, _, err = domain.ParseDecimal("unit_amount_cents", unitRaw); err != nil {
		return
	}
	if tierReqs != nil {
		tiers = make([]domain.RatingTier, len(tierReqs))
		for i, t := range tierReqs {
			tiers[i].UpTo = t.UpTo
			if tiers[i].UnitAmountCents, _, err = domain.ParseDecimal(fmt.Sprintf("tiers[%d].unit_amount_cents", i), t.UnitAmountCents); err != nil {
				return
			}
		}
	}
	pkg, _, err = domain.ParseDecimal("package_amount_cents", pkgRaw)
	return
}
