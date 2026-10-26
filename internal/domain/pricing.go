package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

type PricingMode string

// The price shapes (ADR-130 §3). per_unit bills quantity × unit_amount_cents;
// graduated bills each tier's part of the quantity at the tier's rate;
// package bills ceil(quantity / package_size) × package_amount_cents, so a
// part package bills as a whole one.
const (
	PricingPerUnit   PricingMode = "per_unit"
	PricingGraduated PricingMode = "graduated"
	PricingPackage   PricingMode = "package"
)

type RatingTier struct {
	// UpTo is the tier's upper bound in whole meter units: the tier prices
	// the quantity above the previous tier's bound up to this one. Nil is
	// the open last tier (`"up_to": null`), which prices everything above
	// the previous bound. Only the last tier is open, and it must be.
	UpTo *int64 `json:"up_to"`
	// UnitAmountCents is the per-unit price in cents, carried as a decimal
	// (RateLimits: at most 12 places on write) so sub-cent unit prices (e.g. $3.00 per
	// 1,000,000 tokens = 0.0003 cents/token) bill linearly and exactly. This
	// is the Stripe `unit_amount_decimal` model. Marshals to/from JSON as a
	// string (shopspring/decimal). Invoice line amounts and totals stay whole
	// int64 cents — only the per-unit RATE gains precision.
	UnitAmountCents decimal.Decimal `json:"unit_amount_cents"`
}

// RatingTiers is a rule's graduated tiers. It marshals as [] when there are
// none: a per_unit or package rule, whose stored tiers read back as nil, sent
// null, and strict clients type the field as an array.
type RatingTiers []RatingTier

func (t RatingTiers) MarshalJSON() ([]byte, error) {
	if t == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]RatingTier(t))
}

// PriceShape is how a price bills a quantity: its mode and the mode's
// fields (ADR-130 §3). A plan price, a price version and a customer's
// override each carry one. Every amount is in the currency's minor unit and
// carries decimals (RateLimits on write), so a sub-cent price bills exactly:
// $0.002 per 1,000 tokens is a package of 1000 at "0.2" (Stripe's
// unit_amount_decimal model). PackageSize is a whole number of meter units.
type PriceShape struct {
	Mode               PricingMode     `json:"mode"`
	UnitAmountCents    decimal.Decimal `json:"unit_amount_cents"`
	Tiers              RatingTiers     `json:"tiers"`
	PackageSize        int64           `json:"package_size"`
	PackageAmountCents decimal.Decimal `json:"package_amount_cents"`
}

// Equal reports whether s and o bill every quantity the same: the same mode
// and the same amounts, compared as numbers ("1" equals "1.0").
func (s PriceShape) Equal(o PriceShape) bool {
	if s.Mode != o.Mode || !s.UnitAmountCents.Equal(o.UnitAmountCents) || s.PackageSize != o.PackageSize ||
		!s.PackageAmountCents.Equal(o.PackageAmountCents) || len(s.Tiers) != len(o.Tiers) {
		return false
	}
	for i, t := range s.Tiers {
		u := o.Tiers[i]
		if (t.UpTo == nil) != (u.UpTo == nil) || (t.UpTo != nil && *t.UpTo != *u.UpTo) || !t.UnitAmountCents.Equal(u.UnitAmountCents) {
			return false
		}
	}
	return true
}

// PlanPrice is one usage price of a plan (ADR-130 §1): a meter, the events it
// takes (DimensionMatch) and its content. Its id is stable across content
// edits; the plan, meter and match are its identity and never change. The
// content is the price's version in force at one instant: the instant a
// reader asked for, or from now on for the API.
type PlanPrice struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id,omitempty"`
	PlanID   string `json:"plan_id"`
	MeterID  string `json:"meter_id"`
	// DimensionMatch is the labels an event must carry for the price to
	// take it. {} takes every event of the meter. Values are strings.
	DimensionMatch map[string]string `json:"dimension_match"`
	PriceShape
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is when the content in force was written.
	UpdatedAt time.Time `json:"updated_at"`
}

// Specificity is the number of labels the price names. Of the prices whose
// match an event's labels contain, the one with the most labels takes it.
func (p PlanPrice) Specificity() int { return len(p.DimensionMatch) }

// The limits of a dimension_match (ADR-130 §1), checked at the one store
// writer and by the plan_prices CHECK.
const (
	MaxDimensionKeys      = 16
	MaxDimensionKeyLength = 100
)

// PricesTie reports whether two prices of one plan and meter could both take
// one event with the same number of labels: the same number of keys, and no
// key they share has different values. Such a pair has no winner, so the
// second one written is refused (409 price_conflict). Identical matches tie
// too; the writer reports those as price_exists.
func PricesTie(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		if bv, ok := b[k]; ok && bv != av {
			return false
		}
	}
	return true
}

// SameMatch reports whether two matches name the same labels with the same
// values: the same price identity.
func SameMatch(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		if bv, ok := b[k]; !ok || bv != av {
			return false
		}
	}
	return true
}

// ClaimOrder sorts prices the way an event finds its price: the most labels
// first. Ties are refused when a price is written, so the order between two
// prices of equal specificity never decides an event; it is the order they
// were added in (then the id), so an invoice lists their lines the same way
// on every read.
func ClaimOrder(prices []PlanPrice) {
	sort.SliceStable(prices, func(i, j int) bool {
		if a, b := prices[i].Specificity(), prices[j].Specificity(); a != b {
			return a > b
		}
		if !prices[i].CreatedAt.Equal(prices[j].CreatedAt) {
			return prices[i].CreatedAt.Before(prices[j].CreatedAt)
		}
		return prices[i].ID < prices[j].ID
	})
}

// CustomerPriceOverride is a customer's negotiated price for one plan price
// (ADR-130 §6). It replaces the price's shape for that customer while it is
// in force: [CreatedAt, DeactivatedAt), stamped in the customer's own time
// (wall time, or its test clock's).
type CustomerPriceOverride struct {
	ID         string `json:"id"`
	TenantID   string `json:"tenant_id,omitempty"`
	CustomerID string `json:"customer_id"`
	PriceID    string `json:"price_id"`
	PriceShape
	Reason        string     `json:"reason,omitempty"`
	Active        bool       `json:"active"`
	CreatedAt     time.Time  `json:"created_at"`
	DeactivatedAt *time.Time `json:"deactivated_at,omitempty"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// ApplyTo is the price with the override's shape. The price's id, plan,
// meter and match stay: an override changes the rate, not which events the
// price takes.
func (o CustomerPriceOverride) ApplyTo(p PlanPrice) PlanPrice {
	p.PriceShape = o.PriceShape
	return p
}

type Meter struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id,omitempty"`
	// EventName is the event_name a usage event names this meter by.
	EventName string `json:"event_name"`
	Name      string `json:"name"`
	// Aggregation is how the meter rolls up its events into a period's
	// quantity, one of the Aggregation* values. Every price of the meter
	// bills under it. It cannot change once the meter has events (ADR-130
	// §2).
	Unit        string    `json:"unit"`
	Aggregation string    `json:"aggregation"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// The meter aggregations (ADR-130 §2). A meter has one, and every price of
// the meter bills its events under it: sum adds the quantities, count counts
// the events, max takes the highest quantity, and last takes the latest
// event's quantity, by timestamp and then by id.
const (
	AggregationSum   = "sum"
	AggregationCount = "count"
	AggregationMax   = "max"
	AggregationLast  = "last"
)

// ValidAggregation reports whether agg is one of the meter aggregations.
func ValidAggregation(agg string) bool {
	switch agg {
	case AggregationSum, AggregationCount, AggregationMax, AggregationLast:
		return true
	}
	return false
}

// AdditiveAggregation reports whether usage under agg can be split into
// windows whose quantities add up to the whole: sum and count can, max and
// last cannot (max[0,t) + max[t,end) is not max[0,end)). A spend-threshold
// invoice defers a non-additive bucket to the period close (ADR-066 §4).
func AdditiveAggregation(agg string) bool {
	return agg == AggregationSum || agg == AggregationCount
}

// MeterUnitDisplayScale is the display-scale convention for a meter unit:
// token rates are quoted per 1M tokens (how every AI provider and rate
// card lists them — Anthropic, OpenAI, Metronome); everything else quotes
// per single unit. Returns the decimal shift to apply to a per-unit rate
// and the "per" label. The FE twin is web-v2/src/lib/priceDisplay.ts
// unitScale — the two mappings are deliberately tiny; change them
// together. Only the Go side is test-pinned (TestMeterUnitDisplayScale);
// the TS twin has NO test, so a one-sided edit there fails nothing —
// re-read this before touching either mapping.
func MeterUnitDisplayScale(unit string) (shift int32, per string) {
	switch u := strings.ToLower(strings.TrimSpace(unit)); u {
	case "tokens", "token":
		return 6, "1M tokens"
	default:
		return 0, ""
	}
}

type BillingInterval string

const (
	BillingMonthly BillingInterval = "monthly"
	BillingYearly  BillingInterval = "yearly"
)

type PlanStatus string

const (
	PlanActive   PlanStatus = "active"
	PlanArchived PlanStatus = "archived"
)

// BillTiming controls when the recurring base fee is invoiced relative
// to the period it covers. Usage lines are structurally arrears-only
// (future-period quantities are unknown) so this flag only governs
// the base. See ADR-031.
type BillTiming string

const (
	BillInArrears BillTiming = "in_arrears"
	BillInAdvance BillTiming = "in_advance"
)

// IsValid reports whether t is a recognised bill_timing value.
func (t BillTiming) IsValid() bool {
	return t == BillInArrears || t == BillInAdvance
}

type Plan struct {
	ID              string          `json:"id"`
	TenantID        string          `json:"tenant_id,omitempty"`
	Code            string          `json:"code"`
	Name            string          `json:"name"`
	Description     string          `json:"description,omitempty"`
	Currency        string          `json:"currency"`
	BillingInterval BillingInterval `json:"billing_interval"`
	Status          PlanStatus      `json:"status"`
	BaseAmountCents int64           `json:"base_amount_cents"`
	// BaseBillTiming controls whether the base fee is billed at the
	// start of the period (in_advance) or the end (in_arrears). It is
	// required when the base fee is above 0, and in_arrears when it is 0
	// (ADR-130 §14, ADR-031).
	BaseBillTiming BillTiming `json:"base_bill_timing"`
	// TaxCode optionally overrides the tenant's default_product_tax_code for
	// this plan. Only consulted by the stripe_tax provider. Empty falls
	// back to tenant_settings.default_product_tax_code.
	TaxCode   string    `json:"tax_code,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// UsageClaim is one price's claim on a meter's events, for the usage
// aggregation (usage.Store.AggregateClaims). An event belongs to the first
// claim in the list whose Match its labels contain, so the list is in claim
// order (ClaimOrder); an event no claim takes is the unclaimed bucket.
type UsageClaim struct {
	PriceID string            `json:"price_id"`
	Match   map[string]string `json:"match"`
}

// ClaimsOf is prices as claims, in the order given.
func ClaimsOf(prices []PlanPrice) []UsageClaim {
	out := make([]UsageClaim, 0, len(prices))
	for _, p := range prices {
		match := p.DimensionMatch
		if match == nil {
			match = map[string]string{}
		}
		out = append(out, UsageClaim{PriceID: p.ID, Match: match})
	}
	return out
}

// ClaimedUsage is one bucket of a meter's usage over a window: the events a
// price claimed, rolled up under the meter's aggregation. PriceID "" is the
// unclaimed bucket, the events no price took.
type ClaimedUsage struct {
	PriceID  string          `json:"price_id,omitempty"`
	Quantity decimal.Decimal `json:"quantity"`
}

var ErrInvalidPricingConfig = errors.New("invalid pricing config")

// ErrAmountOverflow is returned when a pricing computation would exceed
// int64. Callers must treat this as a hard failure — a silent wrap would
// emit a negative invoice line in production.
var ErrAmountOverflow = errors.New("amount overflow: exceeds int64 range")

// maxInt64Decimal caches a decimal copy of math.MaxInt64 for the overflow
// guard at the int64-cents conversion boundary.
var maxInt64Decimal = decimal.NewFromInt(math.MaxInt64)

// NominalUnitAmountDecimal is the configured per-unit rate to DISPLAY for a
// price, in DECIMAL CENTS: the unit rate for per_unit, nil for graduated and
// package (which have no single unit rate — callers fall back to the
// effective amount÷quantity). This is the SINGLE source the invoice line stamp
// (billing.nominalRate) and the usage / cost-dashboard price rows both use, so
// every surface renders the same unit price (ADR-054).
func NominalUnitAmountDecimal(rule PriceShape) *decimal.Decimal {
	if rule.Mode != PricingPerUnit {
		return nil
	}
	r := rule.UnitAmountCents
	return &r
}

// ComputeAmountCents prices a decimal quantity through the given price shape
// and returns the result rounded to whole cents. Quantity is decimal so that
// fractional usage primitives (GPU-hours, cached-token ratios) round-trip
// without precision loss. Tier boundaries (`up_to`, `package_size`) stay
// integer because pricing config is authored that way; the math walks tiers
// in decimal space and converts to int64 cents only at the final round, so
// a line is rounded once.
//
// Rounding: half-to-even (banker's rounding) at the cent boundary. This is
// the same convention used elsewhere in the engine (money.RoundHalfToEven)
// and the IEEE 754 default — minimizes systematic bias on bulk invoices.
func ComputeAmountCents(rule PriceShape, quantity decimal.Decimal) (int64, error) {
	if quantity.IsNegative() {
		return 0, ErrInvalidPricingConfig
	}

	switch rule.Mode {
	case PricingPerUnit:
		if rule.UnitAmountCents.IsNegative() {
			return 0, ErrInvalidPricingConfig
		}
		if quantity.IsZero() {
			return 0, nil
		}
		total := quantity.Mul(rule.UnitAmountCents)
		return decimalToCents(total)

	case PricingGraduated:
		if err := CheckTiers(rule.Tiers); err != nil {
			return 0, ErrInvalidPricingConfig
		}
		remaining := quantity
		lastUpper := int64(0)
		amount := decimal.Zero
		for _, tier := range rule.Tiers {
			if tier.UnitAmountCents.IsNegative() {
				return 0, ErrInvalidPricingConfig
			}
			if remaining.IsZero() {
				break
			}
			consumed := remaining
			if tier.UpTo != nil {
				if capDec := decimal.NewFromInt(*tier.UpTo - lastUpper); consumed.GreaterThan(capDec) {
					consumed = capDec
				}
				lastUpper = *tier.UpTo
			}
			amount = amount.Add(consumed.Mul(tier.UnitAmountCents))
			remaining = remaining.Sub(consumed)
		}
		return decimalToCents(amount)

	case PricingPackage:
		if rule.PackageSize <= 0 || rule.PackageAmountCents.IsNegative() {
			return 0, ErrInvalidPricingConfig
		}
		// A part package bills as a whole one: ceil(quantity / size).
		// QuoRem counts the whole packages exactly; Div would round the
		// quotient to 16 places first and could drop a small part.
		packages, part := quantity.QuoRem(decimal.NewFromInt(rule.PackageSize), 0)
		if part.IsPositive() {
			packages = packages.Add(decimal.NewFromInt(1))
		}
		return decimalToCents(packages.Mul(rule.PackageAmountCents))

	default:
		return 0, ErrInvalidPricingConfig
	}
}

// PriceFieldError names the price field CheckPriceShape refused and why.
type PriceFieldError struct {
	Field  string
	Reason string
}

func (e *PriceFieldError) Error() string { return e.Field + ": " + e.Reason }

// CheckPriceShape checks that a price has its mode's fields and only those.
// per_unit uses unit_amount_cents. graduated uses tiers, which must pass
// CheckTiers. package uses package_size, above 0, and package_amount_cents.
// A field the mode does not use must be zero, and tiers empty, so a stored
// price holds only what bills it. Every authoring path runs this one check:
// both API routes, the recipe parser and the recipe save path. The amounts'
// limits are CheckRate's.
func CheckPriceShape(mode PricingMode, unit decimal.Decimal, tiers []RatingTier, pkgSize int64, pkgAmount decimal.Decimal) error {
	switch mode {
	case PricingPerUnit, PricingGraduated, PricingPackage:
	default:
		return &PriceFieldError{Field: "mode", Reason: "must be one of: per_unit, graduated, package"}
	}
	if mode != PricingPerUnit && !unit.IsZero() {
		return &PriceFieldError{Field: "unit_amount_cents", Reason: "used only when mode is per_unit; send 0 or leave it out"}
	}
	if mode != PricingGraduated && len(tiers) != 0 {
		return &PriceFieldError{Field: "tiers", Reason: "used only when mode is graduated; send [] or leave it out"}
	}
	if mode != PricingPackage {
		if pkgSize != 0 {
			return &PriceFieldError{Field: "package_size", Reason: "used only when mode is package; send 0 or leave it out"}
		}
		if !pkgAmount.IsZero() {
			return &PriceFieldError{Field: "package_amount_cents", Reason: "used only when mode is package; send 0 or leave it out"}
		}
	}
	switch mode {
	case PricingGraduated:
		if err := CheckTiers(tiers); err != nil {
			return &PriceFieldError{Field: "tiers", Reason: err.Error()}
		}
	case PricingPackage:
		if pkgSize <= 0 {
			return &PriceFieldError{Field: "package_size", Reason: "package size must be greater than 0"}
		}
	}
	return nil
}

// TierShapeError says what is wrong with a tier table. Index is the tier's
// position, from 0.
type TierShapeError struct {
	Index  int
	Reason string
}

func (e *TierShapeError) Error() string { return e.Reason }

// CheckTiers checks a graduated tier table's shape: at least one tier, each
// bound a whole number above the one before it (the first above 0), and
// only the last tier open (`up_to: null`), which it must be. An open last
// tier is what makes every quantity priceable; a bounded one would leave
// the usage past it with no price and stop the invoice. The rates are
// CheckRate's. Authoring and rating run this one check, so a table the API
// accepts always rates.
func CheckTiers(tiers []RatingTier) error {
	if len(tiers) == 0 {
		return &TierShapeError{Index: -1, Reason: "at least one pricing tier is required"}
	}
	last := len(tiers) - 1
	lastUpper := int64(0)
	for i, t := range tiers {
		if t.UpTo == nil {
			if i != last {
				return &TierShapeError{Index: i, Reason: fmt.Sprintf("tier %d: only the last tier is open (up_to: null); give it an up_to above %d", i+1, lastUpper)}
			}
			continue
		}
		if i == last {
			return &TierShapeError{Index: i, Reason: fmt.Sprintf("tier %d: the last tier must be open (up_to: null), so usage above %d still has a price; to invoice early when usage runs high, set a spend threshold on the subscription", i+1, *t.UpTo)}
		}
		if *t.UpTo <= lastUpper {
			return &TierShapeError{Index: i, Reason: fmt.Sprintf("tier %d: up_to (%d) must be greater than %d", i+1, *t.UpTo, lastUpper)}
		}
		lastUpper = *t.UpTo
	}
	return nil
}

// decimalToCents rounds a decimal cents value (potentially fractional after
// quantity multiplication) to int64 using banker's rounding. Returns
// ErrAmountOverflow if the rounded value would exceed int64 — silent wrap
// would emit a negative invoice line in production, so this is fail-loud.
func decimalToCents(d decimal.Decimal) (int64, error) {
	rounded := d.RoundBank(0)
	// A negative value is < MaxInt64 so it slips past the overflow guard and
	// IntPart() would silently emit a negative invoice line — the exact
	// fail-open this function exists to prevent. Callers only ever reach here
	// with non-negative amounts (quantity and rates are validated >= 0), so a
	// negative here is a logic error: fail loud rather than bill a negative.
	if rounded.IsNegative() {
		return 0, ErrInvalidPricingConfig
	}
	if rounded.GreaterThan(maxInt64Decimal) {
		return 0, ErrAmountOverflow
	}
	return rounded.IntPart(), nil
}

// RatedUsage is one bucket of a subscription's usage as billing prices it:
// the usage of one meter over one stretch of a period that one price of the
// subscription's plan took, or that no price took (PriceID "").
type RatedUsage struct {
	PlanID         string
	MeterID        string
	PriceID        string
	DimensionMatch map[string]string
	Quantity       decimal.Decimal
	AmountCents    int64
	// UnitAmountDecimal is the per-unit price to show (decimal cents): the
	// same value the invoice line shows (ADR-054). Zero on a row no price
	// took.
	UnitAmountDecimal decimal.Decimal
	// BelowZero marks a bucket below zero after corrections: it bills 0.
	BelowZero bool
}

// RatedSubscription is a subscription's usage rated over a window: each
// bucket, every meter its plans price over the window, with usage or not,
// and every plan a row names, as the rater read it. A plan the subscription
// swapped away from in the window is among them, not on its items.
type RatedSubscription struct {
	Usage    []RatedUsage
	MeterIDs []string
	Plans    map[string]Plan
}
