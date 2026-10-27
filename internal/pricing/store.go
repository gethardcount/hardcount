package pricing

import (
	"context"
	"database/sql"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

type Store interface {
	// Meters
	CreateMeter(ctx context.Context, tenantID string, m domain.Meter) (domain.Meter, error)
	CreateMeterTx(ctx context.Context, tx *sql.Tx, tenantID string, m domain.Meter) (domain.Meter, error)
	GetMeter(ctx context.Context, tenantID, id string) (domain.Meter, error)
	GetMeterByEventName(ctx context.Context, tenantID, eventName string) (domain.Meter, error)
	// GetMeterByEventNameTx reads the meter on tx and holds it FOR KEY SHARE
	// until tx ends, so an aggregation PATCH (which takes the meter FOR
	// UPDATE) waits for tx. The recipe install reads the meters it adopts
	// this way (ADR-130 §15).
	GetMeterByEventNameTx(ctx context.Context, tx *sql.Tx, eventName string) (domain.Meter, error)
	// ListMeters returns every meter; ListMetersPage one page of them and
	// the count.
	ListMeters(ctx context.Context, tenantID string) ([]domain.Meter, error)
	ListMetersPage(ctx context.Context, tenantID string, limit, offset int) ([]domain.Meter, int, error)
	// UpdateMeterAudited runs guard, then the UPDATE, then emit, on one
	// transaction (ADR-090 in-tx emission): the patched row and its audit
	// row commit or roll back together. emit receives the UPDATE's
	// RETURNING row and runs only when a row was updated. guard, when
	// non-nil, runs first and aborts the patch on an error.
	UpdateMeterAudited(ctx context.Context, tenantID string, m domain.Meter, guard func(tx *sql.Tx) error, emit func(tx *sql.Tx, out domain.Meter) error) (domain.Meter, error)
	// LockMeterTx locks the meter row FOR UPDATE and returns it as committed
	// now. An aggregation change takes it first, so it waits for every
	// in-flight write of an event of the meter (the event's foreign key
	// holds KEY SHARE on the meter row).
	LockMeterTx(ctx context.Context, tx *sql.Tx, meterID string) (domain.Meter, error)
	// MeterHasUsageTx reports whether the meter has usage events. Once it
	// has, its aggregation is fixed (ADR-130 §2); held rows do not count.
	MeterHasUsageTx(ctx context.Context, tx *sql.Tx, m domain.Meter) (bool, error)
	// MeterHasUsage is MeterHasUsageTx on its own transaction, for reads.
	MeterHasUsage(ctx context.Context, tenantID string, m domain.Meter) (bool, error)

	// Plans
	// CreatePlanWithPrices creates the plan and its prices on one
	// transaction.
	CreatePlanWithPrices(ctx context.Context, tenantID string, p domain.Plan, prices []PriceChange) (domain.Plan, []domain.PlanPrice, error)
	CreatePlanTx(ctx context.Context, tx *sql.Tx, tenantID string, p domain.Plan) (domain.Plan, error)
	GetPlan(ctx context.Context, tenantID, id string) (domain.Plan, error)
	// ListPlans returns every plan; ListPlansPage one page of them and the
	// count.
	ListPlans(ctx context.Context, tenantID string) ([]domain.Plan, error)
	ListPlansPage(ctx context.Context, tenantID string, limit, offset int) ([]domain.Plan, int, error)
	// UpdatePlan locks the plan FOR UPDATE, hands guard the plan as locked
	// and whether a subscription ever used it, and writes what guard
	// returns, all on one transaction.
	UpdatePlan(ctx context.Context, tenantID, id string, guard func(locked domain.Plan, used bool) (domain.Plan, error)) (domain.Plan, error)
	// PlanUsed reports whether any subscription ever referenced the plan
	// (ADR-130 §5).
	PlanUsed(ctx context.Context, tenantID, planID string) (bool, error)

	// Plan prices (price_store.go).
	PriceStore
	// Customer price overrides (override.go).
	OverrideStore
}

// PriceStore holds plan prices, their versions and the test-clock stamps of
// those versions (ADR-130 §1, §7).
type PriceStore interface {
	// WritePrice applies one change to a plan's prices: the chokepoint every
	// price writer goes through. It locks the plan, decides "used", checks
	// the change, writes the version (and clock stamps), ends the overrides
	// of a removed price and runs emit, all in one transaction.
	WritePrice(ctx context.Context, tenantID, planID string, ch PriceChange, emit func(tx *sql.Tx, r PriceWrite) error) (PriceWrite, error)
	// WritePricesTx adds prices to a plan created on tx (POST /v1/plans, the
	// recipe install). The plan is new, so it is never used: every price
	// takes one version from the beginning of time.
	WritePricesTx(ctx context.Context, tx *sql.Tx, planID string, adds []PriceChange) ([]domain.PlanPrice, error)
	// ListPrices returns the prices of the plans that take part from now on,
	// each with its latest content, by plan id.
	ListPrices(ctx context.Context, tenantID string, planIDs []string) (map[string][]domain.PlanPrice, error)
	// PlanPricesAt returns the plan's prices that take part at `at` in the
	// time domain of clockID ("" is wall time), each with its version in
	// force then, in claim order (domain.ClaimOrder) per meter.
	PlanPricesAt(ctx context.Context, tenantID, planID string, at time.Time, clockID string) ([]domain.PlanPrice, error)
	// PlanMeterIDs returns every meter the plan has ever had a price for,
	// removed prices included (ADR-130 §16 of the design: the overlap guard
	// reads the conservative set).
	PlanMeterIDs(ctx context.Context, tenantID, planID string) ([]string, error)
}

// OverrideStore holds customer price overrides (ADR-130 §6).
type OverrideStore interface {
	// WriteOverride closes the customer's active override of the price, if
	// any, and inserts o, after the locks of ADR-130 §8, stamped in the
	// customer's own time.
	WriteOverride(ctx context.Context, tenantID string, o domain.CustomerPriceOverride) (domain.CustomerPriceOverride, error)
	// DeactivateOverride ends the override, stamped like WriteOverride.
	DeactivateOverride(ctx context.Context, tenantID, id string) (domain.CustomerPriceOverride, error)
	// GetOverrideAt returns the customer's override of the price in force at
	// `at` (created_at <= at < deactivated_at), or errs.ErrNotFound.
	GetOverrideAt(ctx context.Context, tenantID, customerID, priceID string, at time.Time) (domain.CustomerPriceOverride, error)
	// ListOverrides returns one page of the active overrides, optionally of
	// one customer and of one price, and how many match.
	ListOverrides(ctx context.Context, tenantID, customerID, priceID string, limit, offset int) ([]domain.CustomerPriceOverride, int, error)
}

// PriceOp is the kind of a price change.
type PriceOp int

const (
	PriceAdd PriceOp = iota
	PriceEdit
	PriceRemove
)

// PriceChange is one change to a plan's prices. An add names the meter and
// match (the identity) and the shape; an edit names the price and the new
// shape; a removal names the price.
type PriceChange struct {
	Op      PriceOp
	PriceID string
	MeterID string
	Match   map[string]string
	Shape   domain.PriceShape
}

// Effective is when a price write applies (ADR-130 §5).
type Effective string

const (
	// EffectiveNow: the plan has never been used, so the write applies at
	// once.
	EffectiveNow Effective = "now"
	// EffectiveNextPeriod: the plan is used, so each subscriber pays the
	// new content from their next billing period.
	EffectiveNextPeriod Effective = "next_period"
)

// EndedOverride is a customer override a price removal ended.
type EndedOverride struct {
	ID         string `json:"id"`
	CustomerID string `json:"customer_id"`
}

// PriceWrite is the result of a price write.
type PriceWrite struct {
	// Price is the price with the content the write gave it; for a removal,
	// the content it had.
	Price     domain.PlanPrice
	Effective Effective
	Removed   bool
	// Before is the content the price had from now on before the write, nil
	// for a new price or a price added back.
	Before *domain.PriceShape
	// ReAdded is true when an add named a removed price's meter and match:
	// the write appended to that price, which keeps its id.
	ReAdded bool
	// EndedOverrides are the overrides a removal ended.
	EndedOverrides []EndedOverride
}
