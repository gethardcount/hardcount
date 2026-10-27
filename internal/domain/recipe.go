package domain

import (
	"time"
)

// RecipeInstance is the DB-backed record of a recipe being instantiated for
// a tenant. The canonical entities a recipe creates (meters, plans and their
// prices) live in their own per-domain tables; RecipeInstance only tracks
// instantiation metadata so we can answer "is this recipe already installed?" idempotently and
// record what it created (CreatedObjects) for the dashboard's install badge.
// (ADR-085: apply is idempotent additive — no force, no uninstall.)
//
// See docs/dev/design/recipes.md.
type RecipeInstance struct {
	ID             string         `json:"id"`
	TenantID       string         `json:"tenant_id,omitempty"`
	RecipeKey      string         `json:"recipe_key"`
	RecipeVersion  string         `json:"recipe_version"`
	Overrides      map[string]any `json:"overrides"`
	CreatedObjects CreatedObjects `json:"created_objects"`
	CreatedAt      time.Time      `json:"created_at"`
	CreatedBy      string         `json:"created_by,omitempty"`
}

// CreatedObjects is the per-role map of entity IDs a recipe apply produced.
// Persisted as JSONB on recipe_instances.created_object_ids and returned in
// the apply response so the dashboard can link to the new plan and meter. It
// is the ledger of what a recipe made: catalog objects only (ADR-130 §15). A
// recipe never creates customers, subscriptions, dunning policies or webhook
// endpoints.
type CreatedObjects struct {
	MeterIDs []string `json:"meter_ids,omitempty"`
	PlanIDs  []string `json:"plan_ids,omitempty"`
	PriceIDs []string `json:"price_ids,omitempty"`
}

// Recipe is the parsed, validated form of a recipe YAML file. Loaded once
// at process boot from the embedded FS and held in the registry; never
// mutated after load. A recipe is catalog only (ADR-130 §15): meters, and
// plans with their prices.
//
// JSON tags use snake_case to match the rest of /v1/* and the wire
// contract in docs/dev/design/recipes.md.
type Recipe struct {
	Key         string           `json:"key"`
	Version     string           `json:"version"`
	Name        string           `json:"name"`
	Summary     string           `json:"summary"`
	Description string           `json:"description,omitempty"`
	Overridable []RecipeOverride `json:"overridable"`

	Meters []RecipeMeter `json:"meters"`
	Plans  []RecipePlan  `json:"plans"`
}

// RecipeOverride is one override key declared in a recipe's `overridable`
// list. Drives validation at instantiate time and the override form in
// the dashboard's preview dialog.
type RecipeOverride struct {
	Key       string   `json:"key"`
	Type      string   `json:"type"` // "string" or "int"
	Default   any      `json:"default"`
	Enum      []string `json:"enum,omitempty"`
	MaxLength int      `json:"max_length,omitempty"`
	Pattern   string   `json:"pattern,omitempty"`
}

type RecipeMeter struct {
	EventName   string `json:"event_name"`
	Name        string `json:"name"`
	Unit        string `json:"unit"`
	Aggregation string `json:"aggregation"`
}

type RecipePlan struct {
	Code            string          `json:"code"`
	Name            string          `json:"name"`
	Currency        string          `json:"currency"`
	BillingInterval BillingInterval `json:"billing_interval"`
	BaseAmountCents int64           `json:"base_amount_cents"`
	// BaseBillTiming is the operator's packaging choice (in_advance /
	// in_arrears). Empty → CreatePlanTx defaults to in_arrears. Honored
	// verbatim on the generated plan; the recipe never second-guesses it.
	BaseBillTiming BillTiming `json:"base_bill_timing,omitempty"`
	// Prices are the plan's usage prices. Each takes the plan's currency.
	Prices []RecipePrice `json:"prices"`
}

// RecipePrice is one usage price of a recipe plan: the recipe meter it is
// on, the labels an event must carry, and its rate.
type RecipePrice struct {
	// Meter is the event_name of the recipe meter the price is on.
	Meter          string            `json:"meter"`
	DimensionMatch map[string]string `json:"dimension_match"`
	PriceShape
}
