package pricing

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// memStore is an in-memory Store for the service and handler tests. It
// models what those layers rely on: ids, tenant scoping and the write each
// call asks for. The plan price chokepoint's locks, "used", the tie check
// and the version rules live in SQL and are tested against Postgres
// (internal/api/plan_prices_*_e2e_test.go, price_store_integration_test.go).
type memStore struct {
	meters    map[string]domain.Meter
	plans     map[string]domain.Plan
	prices    map[string]domain.PlanPrice // by price id
	overrides map[string]domain.CustomerPriceOverride
	// used is what PlanUsed and UpdatePlan's guard answer per plan id.
	used map[string]bool
	// hasUsage is what MeterHasUsageTx answers per meter id. The real read
	// is pinned against Postgres in meter_aggregation_lock_integration_test.go.
	hasUsage map[string]bool
	// writes records every price change WritePrice was handed.
	writes []PriceChange
}

func newMemStore() *memStore {
	return &memStore{
		meters:    map[string]domain.Meter{},
		plans:     map[string]domain.Plan{},
		prices:    map[string]domain.PlanPrice{},
		overrides: map[string]domain.CustomerPriceOverride{},
		used:      map[string]bool{},
		hasUsage:  map[string]bool{},
	}
}

func (m *memStore) CreateMeter(_ context.Context, tenantID string, meter domain.Meter) (domain.Meter, error) {
	for _, existing := range m.meters {
		if existing.TenantID == tenantID && existing.EventName == meter.EventName {
			return domain.Meter{}, fmt.Errorf("%w: meter event_name %q", errs.ErrAlreadyExists, meter.EventName)
		}
	}
	meter.ID = fmt.Sprintf("vlx_mtr_%d", len(m.meters)+1)
	meter.TenantID = tenantID
	m.meters[meter.ID] = meter
	return meter, nil
}

func (m *memStore) CreateMeterTx(ctx context.Context, _ *sql.Tx, tenantID string, meter domain.Meter) (domain.Meter, error) {
	return m.CreateMeter(ctx, tenantID, meter)
}

func (m *memStore) GetMeter(_ context.Context, tenantID, id string) (domain.Meter, error) {
	meter, ok := m.meters[id]
	if !ok || meter.TenantID != tenantID {
		return domain.Meter{}, errs.ErrNotFound
	}
	return meter, nil
}

func (m *memStore) GetMeterByEventName(_ context.Context, tenantID, eventName string) (domain.Meter, error) {
	for _, meter := range m.meters {
		if meter.TenantID == tenantID && meter.EventName == eventName {
			return meter, nil
		}
	}
	return domain.Meter{}, errs.ErrNotFound
}

func (m *memStore) GetMeterByEventNameTx(_ context.Context, _ *sql.Tx, eventName string) (domain.Meter, error) {
	for _, meter := range m.meters {
		if meter.EventName == eventName {
			return meter, nil
		}
	}
	return domain.Meter{}, errs.ErrNotFound
}

func (m *memStore) ListMeters(_ context.Context, tenantID string) ([]domain.Meter, error) {
	var out []domain.Meter
	for _, meter := range m.meters {
		if meter.TenantID == tenantID {
			out = append(out, meter)
		}
	}
	return out, nil
}

// UpdateMeterAudited runs guard, the write and emit, and rolls the write
// back when emit fails, as the aborted transaction would. The real coupling
// is pinned against Postgres in intx_audit_integration_test.go.
func (m *memStore) UpdateMeterAudited(_ context.Context, tenantID string, meter domain.Meter, guard func(tx *sql.Tx) error, emit func(tx *sql.Tx, out domain.Meter) error) (domain.Meter, error) {
	before, ok := m.meters[meter.ID]
	if !ok || before.TenantID != tenantID {
		return domain.Meter{}, errs.ErrNotFound
	}
	if guard != nil {
		if err := guard(nil); err != nil {
			return domain.Meter{}, err
		}
	}
	m.meters[meter.ID] = meter
	if emit != nil {
		if err := emit(nil, meter); err != nil {
			m.meters[meter.ID] = before
			return domain.Meter{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	return meter, nil
}

func (m *memStore) LockMeterTx(_ context.Context, _ *sql.Tx, meterID string) (domain.Meter, error) {
	meter, ok := m.meters[meterID]
	if !ok {
		return domain.Meter{}, errs.ErrNotFound
	}
	return meter, nil
}

func (m *memStore) MeterHasUsageTx(_ context.Context, _ *sql.Tx, meter domain.Meter) (bool, error) {
	return m.hasUsage[meter.ID], nil
}

func (m *memStore) MeterHasUsage(_ context.Context, _ string, meter domain.Meter) (bool, error) {
	return m.hasUsage[meter.ID], nil
}

func (m *memStore) createPlan(tenantID string, p domain.Plan) (domain.Plan, error) {
	for _, existing := range m.plans {
		if existing.TenantID == tenantID && existing.Code == p.Code {
			return domain.Plan{}, fmt.Errorf("%w: plan code %q", errs.ErrAlreadyExists, p.Code)
		}
	}
	p.ID = fmt.Sprintf("vlx_pln_%d", len(m.plans)+1)
	p.TenantID = tenantID
	m.plans[p.ID] = p
	return p, nil
}

func (m *memStore) CreatePlanWithPrices(_ context.Context, tenantID string, p domain.Plan, adds []PriceChange) (domain.Plan, []domain.PlanPrice, error) {
	created, err := m.createPlan(tenantID, p)
	if err != nil {
		return domain.Plan{}, nil, err
	}
	out := make([]domain.PlanPrice, 0, len(adds))
	for _, ch := range adds {
		out = append(out, m.insertPrice(tenantID, created.ID, ch))
	}
	return created, out, nil
}

func (m *memStore) CreatePlanTx(_ context.Context, _ *sql.Tx, tenantID string, p domain.Plan) (domain.Plan, error) {
	return m.createPlan(tenantID, p)
}

func (m *memStore) GetPlan(_ context.Context, tenantID, id string) (domain.Plan, error) {
	p, ok := m.plans[id]
	if !ok || p.TenantID != tenantID {
		return domain.Plan{}, errs.ErrNotFound
	}
	return p, nil
}

func (m *memStore) ListPlans(_ context.Context, tenantID string) ([]domain.Plan, error) {
	var out []domain.Plan
	for _, p := range m.plans {
		if p.TenantID == tenantID {
			out = append(out, p)
		}
	}
	return out, nil
}

func (m *memStore) UpdatePlan(_ context.Context, tenantID, id string, guard func(locked domain.Plan, used bool) (domain.Plan, error)) (domain.Plan, error) {
	p, ok := m.plans[id]
	if !ok || p.TenantID != tenantID {
		return domain.Plan{}, errs.ErrNotFound
	}
	next, err := guard(p, m.used[id])
	if err != nil {
		return domain.Plan{}, err
	}
	m.plans[id] = next
	return next, nil
}

func (m *memStore) PlanUsed(_ context.Context, _, planID string) (bool, error) {
	return m.used[planID], nil
}

func (m *memStore) insertPrice(tenantID, planID string, ch PriceChange) domain.PlanPrice {
	p := domain.PlanPrice{
		ID: fmt.Sprintf("vlx_prc_%d", len(m.prices)+1), TenantID: tenantID, PlanID: planID,
		MeterID: ch.MeterID, DimensionMatch: ch.Match, PriceShape: ch.Shape,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if p.DimensionMatch == nil {
		p.DimensionMatch = map[string]string{}
	}
	m.prices[p.ID] = p
	return p
}

// WritePrice records the change and applies it to the in-memory price. It
// answers "now" or "next_period" from used.
func (m *memStore) WritePrice(_ context.Context, tenantID, planID string, ch PriceChange, emit func(tx *sql.Tx, r PriceWrite) error) (PriceWrite, error) {
	if p, ok := m.plans[planID]; !ok || p.TenantID != tenantID {
		return PriceWrite{}, errs.NotFound("plan")
	}
	m.writes = append(m.writes, ch)
	out := PriceWrite{Effective: EffectiveNow, Removed: ch.Op == PriceRemove}
	if m.used[planID] {
		out.Effective = EffectiveNextPeriod
	}
	switch ch.Op {
	case PriceAdd:
		out.Price = m.insertPrice(tenantID, planID, ch)
	default:
		p, ok := m.prices[ch.PriceID]
		if !ok || p.PlanID != planID {
			return PriceWrite{}, errs.NotFound("price")
		}
		before := p.PriceShape
		out.Before = &before
		if ch.Op == PriceEdit {
			p.PriceShape = ch.Shape
			m.prices[p.ID] = p
		} else {
			delete(m.prices, p.ID)
		}
		out.Price = p
	}
	if emit != nil {
		if err := emit(nil, out); err != nil {
			return PriceWrite{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	return out, nil
}

func (m *memStore) WritePricesTx(_ context.Context, _ *sql.Tx, planID string, adds []PriceChange) ([]domain.PlanPrice, error) {
	p, ok := m.plans[planID]
	if !ok {
		return nil, errs.ErrNotFound
	}
	out := make([]domain.PlanPrice, 0, len(adds))
	for _, ch := range adds {
		out = append(out, m.insertPrice(p.TenantID, planID, ch))
	}
	return out, nil
}

func (m *memStore) ListPrices(_ context.Context, tenantID string, planIDs []string) (map[string][]domain.PlanPrice, error) {
	want := map[string]bool{}
	for _, id := range planIDs {
		want[id] = true
	}
	out := map[string][]domain.PlanPrice{}
	for _, p := range m.prices {
		if p.TenantID == tenantID && want[p.PlanID] {
			out[p.PlanID] = append(out[p.PlanID], p)
		}
	}
	return out, nil
}

func (m *memStore) PlanPricesAt(ctx context.Context, tenantID, planID string, _ time.Time, _ string) ([]domain.PlanPrice, error) {
	byPlan, err := m.ListPrices(ctx, tenantID, []string{planID})
	if err != nil {
		return nil, err
	}
	domain.ClaimOrder(byPlan[planID])
	return byPlan[planID], nil
}

func (m *memStore) PlanMeterIDs(_ context.Context, _, planID string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, p := range m.prices {
		if p.PlanID == planID && !seen[p.MeterID] {
			seen[p.MeterID] = true
			out = append(out, p.MeterID)
		}
	}
	return out, nil
}

func (m *memStore) WriteOverride(_ context.Context, tenantID string, o domain.CustomerPriceOverride) (domain.CustomerPriceOverride, error) {
	if _, ok := m.prices[o.PriceID]; !ok {
		return domain.CustomerPriceOverride{}, errs.Invalid("price_id", fmt.Sprintf("price %q not found", o.PriceID))
	}
	for id, existing := range m.overrides {
		if existing.TenantID == tenantID && existing.CustomerID == o.CustomerID && existing.PriceID == o.PriceID && existing.Active {
			existing.Active = false
			m.overrides[id] = existing
		}
	}
	o.ID = fmt.Sprintf("vlx_cpo_%d", len(m.overrides)+1)
	o.TenantID = tenantID
	o.Active = true
	o.CreatedAt = time.Now().UTC()
	o.UpdatedAt = o.CreatedAt
	m.overrides[o.ID] = o
	return o, nil
}

func (m *memStore) DeactivateOverride(_ context.Context, tenantID, id string) (domain.CustomerPriceOverride, error) {
	o, ok := m.overrides[id]
	if !ok || o.TenantID != tenantID || !o.Active {
		return domain.CustomerPriceOverride{}, errs.ErrNotFound
	}
	o.Active = false
	m.overrides[id] = o
	return o, nil
}

func (m *memStore) GetOverrideAt(_ context.Context, tenantID, customerID, priceID string, _ time.Time) (domain.CustomerPriceOverride, error) {
	for _, o := range m.overrides {
		if o.TenantID == tenantID && o.CustomerID == customerID && o.PriceID == priceID && o.Active {
			return o, nil
		}
	}
	return domain.CustomerPriceOverride{}, errs.ErrNotFound
}

func (m *memStore) ListOverrides(_ context.Context, tenantID, customerID, priceID string) ([]domain.CustomerPriceOverride, error) {
	var out []domain.CustomerPriceOverride
	for _, o := range m.overrides {
		if o.TenantID != tenantID || !o.Active {
			continue
		}
		if (customerID != "" && o.CustomerID != customerID) || (priceID != "" && o.PriceID != priceID) {
			continue
		}
		out = append(out, o)
	}
	return out, nil
}

// seedPlanWithPrice is a plan and one per_unit price on a fresh meter of the
// tenant, for tests that need a price to point at.
func seedPlanWithPrice(m *memStore, tenantID string) (domain.Plan, domain.PlanPrice) {
	meter, _ := m.CreateMeter(context.Background(), tenantID, domain.Meter{EventName: fmt.Sprintf("m%d", len(m.meters)+1), Name: "M", Aggregation: "sum"})
	plan, _ := m.createPlan(tenantID, domain.Plan{Code: fmt.Sprintf("p%d", len(m.plans)+1), Name: "P", Currency: "USD", BillingInterval: domain.BillingMonthly, Status: domain.PlanActive, BaseBillTiming: domain.BillInArrears})
	price := m.insertPrice(tenantID, plan.ID, PriceChange{Op: PriceAdd, MeterID: meter.ID, Shape: domain.PriceShape{Mode: domain.PricingPerUnit}})
	return plan, price
}
