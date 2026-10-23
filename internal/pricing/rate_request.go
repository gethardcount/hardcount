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

// rateTierRequest is one graduated tier on the wire.
type rateTierRequest struct {
	UpTo            int64           `json:"up_to"`
	UnitAmountCents json.RawMessage `json:"unit_amount_cents"`
}

type ratingRuleRequest struct {
	RuleKey                string             `json:"rule_key"`
	Name                   string             `json:"name"`
	Mode                   domain.PricingMode `json:"mode"`
	Currency               string             `json:"currency"`
	FlatAmountCents        json.RawMessage    `json:"flat_amount_cents"`
	GraduatedTiers         []rateTierRequest  `json:"graduated_tiers"`
	PackageSize            int64              `json:"package_size"`
	PackageAmountCents     int64              `json:"package_amount_cents"`
	OverageUnitAmountCents json.RawMessage    `json:"overage_unit_amount_cents"`
}

func (r ratingRuleRequest) input() (CreateRatingRuleInput, error) {
	flat, tiers, overage, err := parseRates(r.FlatAmountCents, r.GraduatedTiers, r.OverageUnitAmountCents)
	if err != nil {
		return CreateRatingRuleInput{}, err
	}
	return CreateRatingRuleInput{
		RuleKey: r.RuleKey, Name: r.Name, Mode: r.Mode, Currency: r.Currency,
		FlatAmountCents: flat, GraduatedTiers: tiers,
		PackageSize: r.PackageSize, PackageAmountCents: r.PackageAmountCents,
		OverageUnitAmountCents: overage,
	}, nil
}

type overrideRequest struct {
	CustomerID             string             `json:"customer_id"`
	RatingRuleVersionID    string             `json:"rating_rule_version_id"`
	Mode                   domain.PricingMode `json:"mode"`
	FlatAmountCents        json.RawMessage    `json:"flat_amount_cents"`
	GraduatedTiers         []rateTierRequest  `json:"graduated_tiers"`
	PackageSize            int64              `json:"package_size"`
	PackageAmountCents     int64              `json:"package_amount_cents"`
	OverageUnitAmountCents json.RawMessage    `json:"overage_unit_amount_cents"`
	Reason                 string             `json:"reason,omitempty"`
}

func (r overrideRequest) input() (CreateOverrideInput, error) {
	flat, tiers, overage, err := parseRates(r.FlatAmountCents, r.GraduatedTiers, r.OverageUnitAmountCents)
	if err != nil {
		return CreateOverrideInput{}, err
	}
	return CreateOverrideInput{
		CustomerID: r.CustomerID, RatingRuleVersionID: r.RatingRuleVersionID, Mode: r.Mode,
		FlatAmountCents: flat, GraduatedTiers: tiers,
		PackageSize: r.PackageSize, PackageAmountCents: r.PackageAmountCents,
		OverageUnitAmountCents: overage, Reason: r.Reason,
	}, nil
}

// parseRates reads the three rates. An absent or null rate is 0, as it was
// when the body decoded straight into decimals: a mode leaves the rates it
// does not use unset. An absent graduated_tiers stays nil.
func parseRates(flatRaw json.RawMessage, tierReqs []rateTierRequest, overageRaw json.RawMessage) (flat decimal.Decimal, tiers []domain.RatingTier, overage decimal.Decimal, err error) {
	if flat, _, err = domain.ParseDecimal("flat_amount_cents", flatRaw); err != nil {
		return
	}
	if tierReqs != nil {
		tiers = make([]domain.RatingTier, len(tierReqs))
		for i, t := range tierReqs {
			tiers[i].UpTo = t.UpTo
			if tiers[i].UnitAmountCents, _, err = domain.ParseDecimal(fmt.Sprintf("graduated_tiers[%d].unit_amount_cents", i), t.UnitAmountCents); err != nil {
				return
			}
		}
	}
	overage, _, err = domain.ParseDecimal("overage_unit_amount_cents", overageRaw)
	return
}
