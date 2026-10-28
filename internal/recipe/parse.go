package recipe

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"github.com/shopspring/decimal"
	"gopkg.in/yaml.v3"

	"github.com/gethardcount/hardcount/internal/domain"
)

// rawRecipe is the shape recipes/*.yaml deserializes into. It mirrors the
// YAML field names; parseRecipe converts it to the canonical domain.Recipe.
// Keeping the wire form separate keeps domain.Recipe free of YAML tags and
// lets us evolve the YAML schema without leaking transient parse fields
// into the rest of the codebase.
//
// A recipe is catalog only (ADR-130 §15): meters, and plans with their
// prices. The removed sections are decoded only so parseRecipe can refuse
// them by name with what replaced them.
type rawRecipe struct {
	Key         string        `yaml:"key"`
	Version     string        `yaml:"version"`
	Name        string        `yaml:"name"`
	Summary     string        `yaml:"summary"`
	Description string        `yaml:"description"`
	Overridable []rawOverride `yaml:"overridable"`
	Meters      []rawMeter    `yaml:"meters"`
	Plans       []rawPlan     `yaml:"plans"`

	RatingRules  any `yaml:"rating_rules"`
	PricingRules any `yaml:"pricing_rules"`
	Dunning      any `yaml:"dunning"`
	Webhook      any `yaml:"webhook"`
}

type rawOverride struct {
	Key       string   `yaml:"key"`
	Type      string   `yaml:"type"`
	Default   any      `yaml:"default"`
	Enum      []string `yaml:"enum"`
	MaxLength int      `yaml:"max_length"`
	Pattern   string   `yaml:"pattern"`
}

type rawMeter struct {
	EventName   string `yaml:"event_name"`
	Name        string `yaml:"name"`
	Unit        string `yaml:"unit"`
	Aggregation string `yaml:"aggregation"`
}

type rawPlan struct {
	Code            string     `yaml:"code"`
	Name            string     `yaml:"name"`
	Currency        string     `yaml:"currency"`
	BillingInterval string     `yaml:"billing_interval"`
	BaseAmountCents int64      `yaml:"base_amount_cents"`
	BaseBillTiming  string     `yaml:"base_bill_timing"`
	Prices          []rawPrice `yaml:"prices"`

	// Meters is decoded only to refuse it: a plan's meters are the meters
	// it has a price for.
	Meters any `yaml:"meters"`
}

// rawPrice is one usage price of a plan. It has no aggregation (the meter
// has one), no priority (the price with the most labels wins) and no
// currency (it takes the plan's).
type rawPrice struct {
	Meter          string         `yaml:"meter"`
	DimensionMatch map[string]any `yaml:"dimension_match"`
	Mode           string         `yaml:"mode"`
	// UnitAmountCents and PackageAmountCents are amounts in cents read as
	// strings (see rawTier) and converted to decimal in parseRecipe.
	UnitAmountCents    string    `yaml:"unit_amount_cents"`
	Tiers              []rawTier `yaml:"tiers"`
	PackageSize        int64     `yaml:"package_size"`
	PackageAmountCents string    `yaml:"package_amount_cents"`

	// Priority and Currency are decoded only to refuse them by name.
	Priority any `yaml:"priority"`
	Currency any `yaml:"currency"`
}

type rawTier struct {
	// UpTo is null (or absent) on the open last tier.
	UpTo *int64 `yaml:"up_to"`
	// Per-unit rate, read as a string so fractional sub-cent rates
	// (e.g. 0.0003 = $3.00/1M tokens) parse losslessly into decimal.Decimal.
	UnitAmountCents string `yaml:"unit_amount_cents"`
}

// removedSections names each section a recipe no longer has and what
// replaced it (ADR-130 §15).
var removedSections = []struct {
	name, instead string
	present       func(r rawRecipe) bool
}{
	{"rating_rules", "put each rate on a price under its plan's prices", func(r rawRecipe) bool { return r.RatingRules != nil }},
	{"pricing_rules", "put each price under its plan's prices", func(r rawRecipe) bool { return r.PricingRules != nil }},
	{"dunning", "a recipe no longer creates a dunning policy; set retries on the Dunning policies page", func(r rawRecipe) bool { return r.Dunning != nil }},
	{"webhook", "a recipe no longer creates a webhook endpoint; add one on the Webhooks page", func(r rawRecipe) bool { return r.Webhook != nil }},
}

// recipeKeyPattern bounds keys to lowercase identifiers so they can ride
// through URLs (`/v1/recipes/{key}`) without escaping.
var recipeKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// parseRate converts a recipe's string-form amount in cents into a decimal.
// Empty is treated as zero (a mode that doesn't use the field). A malformed
// value, or one outside the limits a rate sent to the prices API takes
// (domain.CheckRate), fails recipe load loudly rather than silently pricing
// at zero or failing at install.
func parseRate(where, field, raw string) (decimal.Decimal, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return decimal.Zero, nil
	}
	d, err := domain.ParseDecimalString(field, s)
	if err == nil {
		d, err = domain.CheckRate(field, d)
	}
	if err != nil {
		return decimal.Zero, fmt.Errorf("%s: invalid %s %q: %w", where, field, raw, err)
	}
	return d, nil
}

// parseRecipe reads YAML bytes, validates the structural invariants we
// rely on at runtime, and returns the canonical domain.Recipe. Errors are
// wrapped with the offending field name so a malformed recipe in CI
// surfaces the exact line at boot.
func parseRecipe(data []byte) (domain.Recipe, error) {
	var raw rawRecipe
	// KnownFields rejects any key the raw structs don't declare. Without it a
	// typo such as `base_bill_timming` is silently dropped and the field
	// falls back to its zero value, so the recipe installs a policy its
	// author did not write.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return domain.Recipe{}, fmt.Errorf("yaml decode: %w", err)
	}

	if raw.Key == "" {
		return domain.Recipe{}, fmt.Errorf("missing required field: key")
	}
	if !recipeKeyPattern.MatchString(raw.Key) {
		return domain.Recipe{}, fmt.Errorf("invalid key %q (must match %s)", raw.Key, recipeKeyPattern)
	}
	if raw.Version == "" {
		return domain.Recipe{}, fmt.Errorf("recipe %q: missing version", raw.Key)
	}
	if raw.Name == "" {
		return domain.Recipe{}, fmt.Errorf("recipe %q: missing name", raw.Key)
	}

	out := domain.Recipe{
		Key:         raw.Key,
		Version:     raw.Version,
		Name:        raw.Name,
		Summary:     raw.Summary,
		Description: raw.Description,
	}

	overrideKeys := make(map[string]struct{}, len(raw.Overridable))
	for i, ov := range raw.Overridable {
		if ov.Key == "" {
			return domain.Recipe{}, fmt.Errorf("recipe %q: overridable[%d] missing key", raw.Key, i)
		}
		if _, dup := overrideKeys[ov.Key]; dup {
			return domain.Recipe{}, fmt.Errorf("recipe %q: duplicate overridable key %q", raw.Key, ov.Key)
		}
		overrideKeys[ov.Key] = struct{}{}
		switch ov.Type {
		case "string", "int":
		case "":
			return domain.Recipe{}, fmt.Errorf("recipe %q: overridable %q missing type", raw.Key, ov.Key)
		default:
			return domain.Recipe{}, fmt.Errorf("recipe %q: overridable %q has unsupported type %q", raw.Key, ov.Key, ov.Type)
		}
		out.Overridable = append(out.Overridable, domain.RecipeOverride{
			Key: ov.Key, Type: ov.Type, Default: ov.Default,
			Enum: ov.Enum, MaxLength: ov.MaxLength, Pattern: ov.Pattern,
		})
	}

	meterKeys := make(map[string]struct{}, len(raw.Meters))
	for _, m := range raw.Meters {
		if m.EventName == "" || m.Name == "" {
			return domain.Recipe{}, fmt.Errorf("recipe %q: meter missing event_name or name", raw.Key)
		}
		if _, dup := meterKeys[m.EventName]; dup {
			return domain.Recipe{}, fmt.Errorf("recipe %q: duplicate meter event_name %q", raw.Key, m.EventName)
		}
		if !domain.ValidAggregation(m.Aggregation) {
			return domain.Recipe{}, fmt.Errorf("recipe %q: meter %q aggregation %q is not one of sum, count, max, last", raw.Key, m.EventName, m.Aggregation)
		}
		meterKeys[m.EventName] = struct{}{}
		out.Meters = append(out.Meters, domain.RecipeMeter(m))
	}

	for _, sec := range removedSections {
		if sec.present(raw) {
			return domain.Recipe{}, fmt.Errorf("recipe %q: the %s section is no longer accepted: %s", raw.Key, sec.name, sec.instead)
		}
	}

	for _, p := range raw.Plans {
		if p.Code == "" || p.Name == "" {
			return domain.Recipe{}, fmt.Errorf("recipe %q: plan missing code or name", raw.Key)
		}
		if p.Meters != nil {
			return domain.Recipe{}, fmt.Errorf("recipe %q: plan %q: meters is no longer accepted: a plan's meters are the meters its prices are on", raw.Key, p.Code)
		}
		interval := domain.BillingInterval(p.BillingInterval)
		switch interval {
		case domain.BillingMonthly, domain.BillingYearly:
		default:
			return domain.Recipe{}, fmt.Errorf("recipe %q: plan %q invalid billing_interval %q", raw.Key, p.Code, p.BillingInterval)
		}
		timing := domain.BillTiming(p.BaseBillTiming)
		switch timing {
		case "", domain.BillInArrears, domain.BillInAdvance:
		default:
			return domain.Recipe{}, fmt.Errorf("recipe %q: plan %q invalid base_bill_timing %q (want in_advance or in_arrears)", raw.Key, p.Code, p.BaseBillTiming)
		}
		plan := domain.RecipePlan{
			Code: p.Code, Name: p.Name, Currency: p.Currency, BillingInterval: interval,
			BaseAmountCents: p.BaseAmountCents, BaseBillTiming: timing,
		}
		for i, rp := range p.Prices {
			price, err := parsePrice(fmt.Sprintf("recipe %q: plan %q: prices[%d]", raw.Key, p.Code, i), rp, meterKeys)
			if err != nil {
				return domain.Recipe{}, err
			}
			// The tie check the price writer runs (ADR-130 §4), so a recipe
			// whose prices have no winner fails the load, not every install.
			for j, prev := range plan.Prices {
				if prev.Meter != price.Meter {
					continue
				}
				if domain.SameMatch(prev.DimensionMatch, price.DimensionMatch) {
					return domain.Recipe{}, fmt.Errorf("recipe %q: plan %q: prices[%d] and prices[%d] have the same labels %v", raw.Key, p.Code, j, i, price.DimensionMatch)
				}
				if domain.PricesTie(prev.DimensionMatch, price.DimensionMatch) {
					return domain.Recipe{}, fmt.Errorf("recipe %q: plan %q: prices[%d] %v and prices[%d] %v could take the same events with the same number of labels, so neither would win", raw.Key, p.Code, j, prev.DimensionMatch, i, price.DimensionMatch)
				}
			}
			plan.Prices = append(plan.Prices, price)
		}
		out.Plans = append(out.Plans, plan)
	}

	if err := validateTemplateReferences(out, overrideKeys); err != nil {
		return domain.Recipe{}, err
	}

	return out, nil
}

// parsePrice checks one price of a plan: a meter the recipe declares, labels
// that are non-empty strings, and a shape the prices API accepts.
func parsePrice(where string, rp rawPrice, meters map[string]struct{}) (domain.RecipePrice, error) {
	if rp.Priority != nil {
		return domain.RecipePrice{}, fmt.Errorf("%s: priority is no longer accepted: the price with the most labels takes an event", where)
	}
	if rp.Currency != nil {
		return domain.RecipePrice{}, fmt.Errorf("%s: currency is no longer accepted: a price takes its plan's currency", where)
	}
	if _, ok := meters[rp.Meter]; !ok {
		return domain.RecipePrice{}, fmt.Errorf("%s: meter %q is not one of the recipe's meters", where, rp.Meter)
	}
	// Labels are strings (ADR-130 §1). An empty key or value, or a nested
	// one, can never match an event's labels, so the price would never bill.
	// The vocabulary is open (operators send their own labels); the built-in
	// catalogs' agreement with the LiteLLM mapper is checked by its tests.
	match := make(map[string]string, len(rp.DimensionMatch))
	for k, v := range rp.DimensionMatch {
		if strings.TrimSpace(k) == "" {
			return domain.RecipePrice{}, fmt.Errorf("%s: dimension_match has an empty key", where)
		}
		val, ok := v.(string)
		if !ok {
			return domain.RecipePrice{}, fmt.Errorf("%s: dimension_match[%q] must be a string, got %T", where, k, v)
		}
		if strings.TrimSpace(val) == "" {
			return domain.RecipePrice{}, fmt.Errorf("%s: dimension_match[%q] is empty", where, k)
		}
		match[k] = val
	}
	unit, err := parseRate(where, "unit_amount_cents", rp.UnitAmountCents)
	if err != nil {
		return domain.RecipePrice{}, err
	}
	pkg, err := parseRate(where, "package_amount_cents", rp.PackageAmountCents)
	if err != nil {
		return domain.RecipePrice{}, err
	}
	shape := domain.PriceShape{
		Mode:               domain.PricingMode(rp.Mode),
		UnitAmountCents:    unit,
		PackageSize:        rp.PackageSize,
		PackageAmountCents: pkg,
	}
	for _, t := range rp.Tiers {
		ua, err := parseRate(where, "tiers.unit_amount_cents", t.UnitAmountCents)
		if err != nil {
			return domain.RecipePrice{}, err
		}
		shape.Tiers = append(shape.Tiers, domain.RatingTier{UpTo: t.UpTo, UnitAmountCents: ua})
	}
	// The shape check the prices API runs: the mode's fields and only
	// those, and a tier table that rates.
	if err := domain.CheckPriceShape(shape.Mode, shape.UnitAmountCents, shape.Tiers, shape.PackageSize, shape.PackageAmountCents); err != nil {
		return domain.RecipePrice{}, fmt.Errorf("%s: %w", where, err)
	}
	return domain.RecipePrice{Meter: rp.Meter, DimensionMatch: match, PriceShape: shape}, nil
}
