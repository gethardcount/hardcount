package domain

import (
	"encoding/json"
	"time"

	"github.com/shopspring/decimal"
)

// UsageEventOrigin is the ingest path that produced a usage_events row.
// 'api' covers real-time POST /usage-events (and /batch). 'backfill' covers
// the explicit historical-ingest endpoint. Extend it as new sources appear.
type UsageEventOrigin string

const (
	UsageOriginAPI      UsageEventOrigin = "api"
	UsageOriginBackfill UsageEventOrigin = "backfill"
)

// UsageEvent is one metered usage event. Its Quantity is a NUMERIC(38, 12)
// decimal, so AI usage primitives (GPU-hours, cached-token ratios, partial
// KV-cache reads) round-trip exactly. Quantity maps to Stripe's
// quantity_decimal. A response sends it as a JSON string ("0.50"). On a
// request, the unmarshaller accepts both a number (5) and a string ("5.5").
type UsageEvent struct {
	ID             string           `json:"id"`
	TenantID       string           `json:"tenant_id,omitempty"`
	CustomerID     string           `json:"customer_id"`
	MeterID        string           `json:"meter_id"`
	Quantity       decimal.Decimal  `json:"quantity"`
	Dimensions     map[string]any   `json:"dimensions,omitempty"`
	IdempotencyKey string           `json:"idempotency_key,omitempty"`
	Timestamp      time.Time        `json:"timestamp"`
	Origin         UsageEventOrigin `json:"origin,omitempty"`

	// ProviderCostMicros is the cost-of-goods-sold (COGS) stamp (ADR-079):
	// what serving this event cost the operator on their provider bill, in
	// micro-dollars (1e-6). It is resolved from provider_cost_rates at ingest
	// time, as a snapshot, so later rate edits never rewrite it. Nil means
	// unresolved, or a row from before the feature. Unresolved is an event
	// with costable dimensions but no matching rate (the signal to add a
	// rate), or a cost that does not fit BIGINT micros. It is for the
	// operator only. The API sends it as provider_cost_cents (MarshalJSON).
	ProviderCostMicros *int64 `json:"-"`
	// ProviderCostSource is 'table' (inferred from provider_cost_rates),
	// 'not_applicable' (no costable dimensions, as on meters that do not
	// count tokens) or 'observed'. 'observed' is the sender's own per-half
	// cost (ADR-079 D4), and it wins over inference. usage/postgres.go stamps
	// it when ObservedCostMicros is set. Empty means unresolved, or a row
	// from before the feature.
	ProviderCostSource string `json:"provider_cost_source,omitempty"`

	// BillingStatus is "after_close" on the answer to a write whose event
	// falls in a period already invoiced: the event is stored but billed on
	// no invoice (ADR-120). It is set only on the write's own 201. It is
	// never stored, nor set on a replay or a read, because a later
	// recomputation could call a row the close did bill "after_close".
	// Otherwise it is absent, which promises nothing.
	BillingStatus string `json:"billing_status,omitempty"`
}

// MarshalJSON sends the provider cost as provider_cost_cents, a decimal
// string in cents. Every *_cents field is in the currency's minor unit
// (ADR-130 §3). The stored micro-dollars convert exactly:
// cents = micros ÷ 10^4.
func (e UsageEvent) MarshalJSON() ([]byte, error) {
	type wire UsageEvent
	var cents *decimal.Decimal
	if e.ProviderCostMicros != nil {
		c := MicrosToCents(*e.ProviderCostMicros)
		cents = &c
	}
	return json.Marshal(struct {
		wire
		ProviderCostCents *decimal.Decimal `json:"provider_cost_cents,omitempty"`
	}{wire(e), cents})
}

// MicrosToCents converts micro-dollars, the unit provider costs are stored
// in, to cents: exactly micros × 10^-4.
func MicrosToCents(micros int64) decimal.Decimal {
	return decimal.New(micros, -4)
}

// ProviderCostRate is one row of the operator's provider cost table
// (ADR-079): what the operator pays a provider per token, for one (provider,
// model, token_type). UnitCostCents is in cents per token, as a decimal
// (ADR-130 §3, the unit rule every *_cents field follows). Real rates reach
// 0.00015 cents a token, so it is never a float. The table holds the current
// rate: one row per key per mode, edited in place. The stamp on each event is
// the historical snapshot, so an edit never rewrites history.
type ProviderCostRate struct {
	ID            string          `json:"id"`
	TenantID      string          `json:"tenant_id,omitempty"`
	Provider      string          `json:"provider"`
	Model         string          `json:"model"`
	TokenType     string          `json:"token_type"`
	UnitCostCents decimal.Decimal `json:"unit_cost_cents"`
	Currency      string          `json:"currency"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// HeldUsageReason says why a usage row was held instead of ingested.
type HeldUsageReason string

const (
	HeldReasonCustomerNotFound HeldUsageReason = "customer_not_found"
	HeldReasonMeterNotFound    HeldUsageReason = "meter_not_found"
)

// HeldUsageStatus is a held row's state. It moves held -> released or
// held -> discarded, never back.
type HeldUsageStatus string

const (
	HeldStatusHeld      HeldUsageStatus = "held"
	HeldStatusReleased  HeldUsageStatus = "released"
	HeldStatusDiscarded HeldUsageStatus = "discarded"
)

// HeldUsageSourceLiteLLM is the only source that holds rows today: the
// LiteLLM callback, which must answer 200 for a row it cannot ingest.
const HeldUsageSourceLiteLLM = "litellm"

// HeldUsageEvent is a usage row Hardcount received but could not ingest yet,
// because its customer or meter did not exist (#1044, ADR-122). It keeps
// the idempotency key the row would have had in usage_events, so a release
// writes it at most once.
type HeldUsageEvent struct {
	ID                 string          `json:"id"`
	Source             string          `json:"source"`
	ExternalCustomerID string          `json:"external_customer_id"`
	EventName          string          `json:"event_name"`
	Quantity           decimal.Decimal `json:"quantity"`
	Dimensions         map[string]any  `json:"dimensions,omitempty"`
	IdempotencyKey     string          `json:"idempotency_key"`
	EventAt            time.Time       `json:"event_at"`
	ObservedCostMicros *int64          `json:"observed_cost_micros,omitempty"`
	Reason             HeldUsageReason `json:"reason"`
	ReceivedAt         time.Time       `json:"received_at"`
	Status             HeldUsageStatus `json:"status"`
}
