package pricing

import (
	"encoding/json"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// The price, plan and price-override bodies keep their rates raw, so every
// bad rate is a 422 naming the field (domain.ParseDecimal) rather than a 400
// for the whole body, and an exponent too large to read has its own message.
// The limits are the service's (checkRates), so a Go caller such as the
// recipe installer gets the same ones.

// rateTierRequest is one graduated tier on the wire. up_to is a whole number,
// or null for the open last tier.
type rateTierRequest struct {
	UpTo            *int64          `json:"up_to"`
	UnitAmountCents json.RawMessage `json:"unit_amount_cents"`
}

// shapeRequest is a price shape on the wire.
type shapeRequest struct {
	Mode               domain.PricingMode `json:"mode"`
	UnitAmountCents    json.RawMessage    `json:"unit_amount_cents"`
	Tiers              []rateTierRequest  `json:"tiers"`
	PackageSize        int64              `json:"package_size"`
	PackageAmountCents json.RawMessage    `json:"package_amount_cents"`
}

func (r shapeRequest) shape(prefix string) (domain.PriceShape, error) {
	unit, tiers, pkg, err := parseRates(r.UnitAmountCents, r.Tiers, r.PackageAmountCents)
	if err != nil {
		return domain.PriceShape{}, prefixField(prefix, err)
	}
	return domain.PriceShape{
		Mode: r.Mode, UnitAmountCents: unit, Tiers: tiers,
		PackageSize: r.PackageSize, PackageAmountCents: pkg,
	}, nil
}

// priceRequest is POST /v1/plans/{plan_id}/prices and one entry of
// POST /v1/plans's prices.
type priceRequest struct {
	MeterID        string         `json:"meter_id"`
	DimensionMatch map[string]any `json:"dimension_match"`
	shapeRequest
}

func (r priceRequest) input(prefix string) (PriceInput, error) {
	match := make(map[string]string, len(r.DimensionMatch))
	for k, v := range r.DimensionMatch {
		s, ok := v.(string)
		if !ok {
			return PriceInput{}, errs.Invalid(prefix+"dimension_match", fmt.Sprintf("label %q must have a string value (got %s); write numbers and booleans as strings", k, jsonKind(v)))
		}
		match[k] = s
	}
	shape, err := r.shape(prefix)
	if err != nil {
		return PriceInput{}, err
	}
	return PriceInput{MeterID: r.MeterID, DimensionMatch: match, PriceShape: shape}, nil
}

// jsonKind names the JSON type of a decoded value.
func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case float64, json.Number:
		return "a number"
	case map[string]any:
		return "an object"
	case []any:
		return "an array"
	}
	return fmt.Sprintf("%T", v)
}

// priceEditRequest is PATCH /v1/plans/{plan_id}/prices/{id}: content only.
// meter_id and dimension_match are decoded only to refuse them by name: they
// are the price's identity.
type priceEditRequest struct {
	MeterID        *string `json:"meter_id"`
	DimensionMatch any     `json:"dimension_match"`
	shapeRequest
}

func (r priceEditRequest) shape() (domain.PriceShape, error) {
	if r.MeterID != nil {
		return domain.PriceShape{}, errs.Invalid("meter_id", "is a price's identity and cannot change; add a price on the other meter and remove this one")
	}
	if r.DimensionMatch != nil {
		return domain.PriceShape{}, errs.Invalid("dimension_match", "is a price's identity and cannot change; add a price with the other dimension_match and remove this one")
	}
	return r.shapeRequest.shape("")
}

// planRequest is POST /v1/plans.
type planRequest struct {
	Code            string                 `json:"code"`
	Name            string                 `json:"name"`
	Description     string                 `json:"description,omitempty"`
	Currency        string                 `json:"currency"`
	BillingInterval domain.BillingInterval `json:"billing_interval"`
	BaseAmountCents int64                  `json:"base_amount_cents"`
	BaseBillTiming  domain.BillTiming      `json:"base_bill_timing,omitempty"`
	Prices          []priceRequest         `json:"prices"`
	Status          string                 `json:"status,omitempty"`
	TaxCode         string                 `json:"tax_code,omitempty"`
}

func (r planRequest) input() (CreatePlanInput, error) {
	prices := make([]PriceInput, 0, len(r.Prices))
	for i, p := range r.Prices {
		in, err := p.input(fmt.Sprintf("prices[%d].", i))
		if err != nil {
			return CreatePlanInput{}, err
		}
		prices = append(prices, in)
	}
	return CreatePlanInput{
		Code: r.Code, Name: r.Name, Description: r.Description, Currency: r.Currency,
		BillingInterval: r.BillingInterval, BaseAmountCents: r.BaseAmountCents,
		BaseBillTiming: r.BaseBillTiming, Prices: prices, Status: r.Status, TaxCode: r.TaxCode,
	}, nil
}

type overrideRequest struct {
	CustomerID string `json:"customer_id"`
	PriceID    string `json:"price_id"`
	shapeRequest
	Reason string `json:"reason,omitempty"`
}

func (r overrideRequest) input() (CreateOverrideInput, error) {
	shape, err := r.shape("")
	if err != nil {
		return CreateOverrideInput{}, err
	}
	return CreateOverrideInput{CustomerID: r.CustomerID, PriceID: r.PriceID, PriceShape: shape, Reason: r.Reason}, nil
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
