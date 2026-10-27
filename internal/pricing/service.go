package pricing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

var slugPattern = regexp.MustCompile(`^[a-zA-Z0-9_\-]+$`)

// AuditEmitter is the narrow in-tx audit seam (ADR-090). The service builds
// the emission content at the mutation site; the store threads the closure
// onto its transaction so the pricing write and the audit row share fate.
type AuditEmitter interface {
	LogInTx(ctx context.Context, tx *sql.Tx, e audit.Entry) error
}

// SubscriptionPlanUsageReader counts subscriptions referencing a plan.
// UpdatePlan uses it to keep the base fee fixed while live subscriptions use
// the plan (ADR-034). Narrow shape; the concrete query lives in
// *subscription.PostgresStore.
//
// "Live" = any sub that may still bill: every status but canceled.
type SubscriptionPlanUsageReader interface {
	CountLiveSubsByPlan(ctx context.Context, tenantID, planID string) (int, error)
}

// CustomerReader fetches a customer by id on a tenant- and mode-scoped
// transaction. Implemented by *customer.PostgresStore, wired in router.go.
type CustomerReader interface {
	Get(ctx context.Context, tenantID, customerID string) (domain.Customer, error)
}

type Service struct {
	store        Store
	subPlanUsage SubscriptionPlanUsageReader
	auditLogger  AuditEmitter
	customers    CustomerReader
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

// SetAuditLogger wires in-tx audit emission for the pricing mutations that
// own their transaction in the store: meter PATCH and every price write
// (ADR-090). Nil-safe — an unwired service skips emission, which keeps the
// narrow unit tests fake-friendly. The handler's post-hoc AuditWriter (plan
// and meter create, plan PATCH, override) is a separate seam.
func (s *Service) SetAuditLogger(a AuditEmitter) { s.auditLogger = a }

// SetCustomerReader wires the customer read CreateOverride runs before
// writing. When unwired (narrow unit tests) the read is skipped.
func (s *Service) SetCustomerReader(r CustomerReader) { s.customers = r }

// SetSubscriptionPlanUsageReader wires the live-sub counter UpdatePlan uses
// to keep the base fee fixed (ADR-034). Optional — when unwired (narrow unit
// tests) the base fee is editable. Production wires
// *subscription.PostgresStore via router.go.
func (s *Service) SetSubscriptionPlanUsageReader(r SubscriptionPlanUsageReader) {
	s.subPlanUsage = r
}

// ---------------------------------------------------------------------------
// Meters
// ---------------------------------------------------------------------------

type CreateMeterInput struct {
	// EventName is the event_name usage events send for this meter.
	EventName   string `json:"event_name"`
	Name        string `json:"name"`
	Unit        string `json:"unit"`
	Aggregation string `json:"aggregation"`
}

func (s *Service) CreateMeter(ctx context.Context, tenantID string, input CreateMeterInput) (domain.Meter, error) {
	m, err := meterFromInput(input)
	if err != nil {
		return domain.Meter{}, err
	}
	return s.store.CreateMeter(ctx, tenantID, m)
}

// CreateMeterTx is CreateMeter on the caller's transaction (the recipe
// install), with the same checks.
func (s *Service) CreateMeterTx(ctx context.Context, tx *sql.Tx, tenantID string, m domain.Meter) (domain.Meter, error) {
	checked, err := meterFromInput(CreateMeterInput{EventName: m.EventName, Name: m.Name, Unit: m.Unit, Aggregation: m.Aggregation})
	if err != nil {
		return domain.Meter{}, err
	}
	return s.store.CreateMeterTx(ctx, tx, tenantID, checked)
}

func meterFromInput(input CreateMeterInput) (domain.Meter, error) {
	eventName := strings.TrimSpace(input.EventName)
	name := strings.TrimSpace(input.Name)
	if eventName == "" {
		return domain.Meter{}, errs.Required("event_name")
	}
	if err := domain.MaxLen("event_name", eventName, 100); err != nil {
		return domain.Meter{}, err
	}
	if !slugPattern.MatchString(eventName) {
		return domain.Meter{}, errs.Invalid("event_name", "must contain only alphanumeric characters, hyphens, and underscores")
	}
	if name == "" {
		return domain.Meter{}, errs.Required("name")
	}
	if err := domain.MaxLen("name", name, 255); err != nil {
		return domain.Meter{}, err
	}
	unit := strings.TrimSpace(input.Unit)
	if unit == "" {
		unit = "unit"
	}
	agg := input.Aggregation
	if agg == "" {
		agg = domain.AggregationSum
	}
	if !domain.ValidAggregation(agg) {
		return domain.Meter{}, errs.Invalid("aggregation", "must be one of: sum, count, max, last")
	}
	return domain.Meter{EventName: eventName, Name: name, Unit: unit, Aggregation: agg}, nil
}

// UpdateMeterInput is the PATCH shape: nil = leave unchanged.
type UpdateMeterInput struct {
	Name        *string `json:"name,omitempty"`
	Unit        *string `json:"unit,omitempty"`
	Aggregation *string `json:"aggregation,omitempty"`
}

// UpdateMeter patches a meter's name, unit or aggregation. A meter only
// counts (ADR-130 §1): its prices are on the plans. Name and unit change at
// once. The aggregation is fixed once the meter has events (ADR-130 §2):
// every event so far was counted under it.
//
// The UPDATE writes every column from the read below, so the write locks the
// meter and refuses if another write changed it since that read; otherwise a
// rename could put back an aggregation another PATCH just changed.
func (s *Service) UpdateMeter(ctx context.Context, tenantID, id string, input UpdateMeterInput) (domain.Meter, error) {
	m, err := s.store.GetMeter(ctx, tenantID, id)
	if err != nil {
		return domain.Meter{}, err
	}
	before := m
	if input.Name != nil {
		name := strings.TrimSpace(*input.Name)
		if name == "" {
			return domain.Meter{}, errs.Required("name")
		}
		if err := domain.MaxLen("name", name, 255); err != nil {
			return domain.Meter{}, err
		}
		m.Name = name
	}
	if input.Unit != nil {
		unit := strings.TrimSpace(*input.Unit)
		if unit == "" {
			unit = "unit"
		}
		m.Unit = unit
	}
	if input.Aggregation != nil {
		agg := *input.Aggregation
		if !domain.ValidAggregation(agg) {
			return domain.Meter{}, errs.Invalid("aggregation", "must be one of: sum, count, max, last")
		}
		m.Aggregation = agg
	}
	want := m
	guard := func(tx *sql.Tx) error {
		locked, err := s.store.LockMeterTx(ctx, tx, m.ID)
		if err != nil {
			return err
		}
		if locked.Name != before.Name || locked.Unit != before.Unit || locked.Aggregation != before.Aggregation {
			return errs.InvalidState("This meter changed while the request was running. Refresh the page and try again.")
		}
		if want.Aggregation != locked.Aggregation {
			return s.refuseAggregationChange(ctx, tx, locked)
		}
		return nil
	}
	return s.store.UpdateMeterAudited(ctx, tenantID, m, guard, s.meterUpdateEmission(ctx, input))
}

// refuseAggregationChange refuses to change the locked meter's aggregation
// once it has usage events (ADR-130 §2). Held rows do not count: they have
// never been billed. It runs after LockMeterTx, whose FOR UPDATE waits for
// any write of an event of the meter, an ingest or a held-usage release (the
// event's foreign key holds KEY SHARE on the meter row), so an event that
// commits first is seen here, and one written later waits for this commit.
func (s *Service) refuseAggregationChange(ctx context.Context, tx *sql.Tx, locked domain.Meter) error {
	has, err := s.store.MeterHasUsageTx(ctx, tx, locked)
	if err != nil {
		return err
	}
	if has {
		return errs.InvalidState(fmt.Sprintf("Meter %q has usage events, so its aggregation stays %s: its events are counted under it. To count differently, make a new meter with another event name.", locked.EventName, locked.Aggregation)).WithCode(errs.CodeMeterHasUsage)
	}
	return nil
}

// meterUpdateEmission builds the in-tx audit emission for a meter PATCH.
// Wire strings (action "update", resource "meter") are FROZEN — they match
// the meter CREATE row, so a meter's audit timeline stays one resource.
//
// Metadata records the fields the REQUEST actually set, valued from the
// UPDATE's RETURNING row (read inside the tx), never a diff against a pre-tx
// read that a concurrent PATCH could have made untrue.
func (s *Service) meterUpdateEmission(ctx context.Context, input UpdateMeterInput) func(tx *sql.Tx, out domain.Meter) error {
	if s.auditLogger == nil {
		return nil
	}
	return func(tx *sql.Tx, out domain.Meter) error {
		meta := map[string]any{"event_name": out.EventName}
		if input.Name != nil {
			meta["name"] = out.Name
		}
		if input.Unit != nil {
			meta["unit"] = out.Unit
		}
		if input.Aggregation != nil {
			meta["aggregation"] = out.Aggregation
		}
		return s.auditLogger.LogInTx(ctx, tx, audit.Entry{
			Action:        domain.AuditActionUpdate,
			ResourceType:  "meter",
			ResourceID:    out.ID,
			ResourceLabel: out.Name,
			Metadata:      meta,
		})
	}
}

func (s *Service) GetMeter(ctx context.Context, tenantID, id string) (domain.Meter, error) {
	return s.store.GetMeter(ctx, tenantID, id)
}

// GetMeterByEventName returns the meter usage events name by eventName.
func (s *Service) GetMeterByEventName(ctx context.Context, tenantID, eventName string) (domain.Meter, error) {
	return s.store.GetMeterByEventName(ctx, tenantID, eventName)
}

// GetMeterByEventNameTx reads the meter on tx FOR KEY SHARE (the recipe
// install's adoption read).
func (s *Service) GetMeterByEventNameTx(ctx context.Context, tx *sql.Tx, eventName string) (domain.Meter, error) {
	return s.store.GetMeterByEventNameTx(ctx, tx, eventName)
}

// MeterHasUsage reports whether the meter has usage events, which fix its
// aggregation (ADR-130 §2). Held rows do not count.
func (s *Service) MeterHasUsage(ctx context.Context, tenantID string, m domain.Meter) (bool, error) {
	return s.store.MeterHasUsage(ctx, tenantID, m)
}

func (s *Service) ListMeters(ctx context.Context, tenantID string) ([]domain.Meter, error) {
	return s.store.ListMeters(ctx, tenantID)
}

// ---------------------------------------------------------------------------
// Plans
// ---------------------------------------------------------------------------

type CreatePlanInput struct {
	Code            string                 `json:"code"`
	Name            string                 `json:"name"`
	Description     string                 `json:"description,omitempty"`
	Currency        string                 `json:"currency"`
	BillingInterval domain.BillingInterval `json:"billing_interval"`
	BaseAmountCents int64                  `json:"base_amount_cents"`
	// BaseBillTiming is required when BaseAmountCents is above 0. With a
	// base of 0 it may be left out and is stored in_arrears (ADR-130 §14).
	BaseBillTiming domain.BillTiming `json:"base_bill_timing,omitempty"`
	// Prices are the plan's usage prices, created with it.
	Prices []PriceInput `json:"prices"`
	// Status is optional and may only be "active": create always makes an
	// active plan. It stays a field so a caller sending "active" is not
	// refused as an unknown field.
	Status  string `json:"status,omitempty"`
	TaxCode string `json:"tax_code,omitempty"`
}

// CreatePlan creates the plan and its prices in one transaction (ADR-130
// §16).
func (s *Service) CreatePlan(ctx context.Context, tenantID string, input CreatePlanInput) (domain.Plan, []domain.PlanPrice, error) {
	p, err := planFromInput(input)
	if err != nil {
		return domain.Plan{}, nil, err
	}
	adds, err := priceAdds(input.Prices)
	if err != nil {
		return domain.Plan{}, nil, err
	}
	return s.store.CreatePlanWithPrices(ctx, tenantID, p, adds)
}

// CreatePlanWithPricesTx creates a plan with its prices on the caller's
// transaction (the recipe install), with the checks CreatePlan runs.
func (s *Service) CreatePlanWithPricesTx(ctx context.Context, tx *sql.Tx, tenantID string, plan domain.Plan, prices []domain.PlanPrice) (domain.Plan, []domain.PlanPrice, error) {
	input := CreatePlanInput{
		Code: plan.Code, Name: plan.Name, Description: plan.Description, Currency: plan.Currency,
		BillingInterval: plan.BillingInterval, BaseAmountCents: plan.BaseAmountCents,
		BaseBillTiming: plan.BaseBillTiming, Status: string(plan.Status), TaxCode: plan.TaxCode,
	}
	for _, p := range prices {
		input.Prices = append(input.Prices, PriceInput{MeterID: p.MeterID, DimensionMatch: p.DimensionMatch, PriceShape: p.PriceShape})
	}
	p, err := planFromInput(input)
	if err != nil {
		return domain.Plan{}, nil, err
	}
	adds, err := priceAdds(input.Prices)
	if err != nil {
		return domain.Plan{}, nil, err
	}
	created, err := s.store.CreatePlanTx(ctx, tx, tenantID, p)
	if err != nil {
		return domain.Plan{}, nil, err
	}
	written, err := s.store.WritePricesTx(ctx, tx, created.ID, adds)
	if err != nil {
		return domain.Plan{}, nil, err
	}
	return created, written, nil
}

func planFromInput(input CreatePlanInput) (domain.Plan, error) {
	code := strings.TrimSpace(input.Code)
	name := strings.TrimSpace(input.Name)
	currency := strings.ToUpper(strings.TrimSpace(input.Currency))

	if code == "" {
		return domain.Plan{}, errs.Required("code")
	}
	if err := domain.MaxLen("code", code, 100); err != nil {
		return domain.Plan{}, err
	}
	if !slugPattern.MatchString(code) {
		return domain.Plan{}, errs.Invalid("code", "must contain only alphanumeric characters, hyphens, and underscores")
	}
	if name == "" {
		return domain.Plan{}, errs.Required("name")
	}
	if err := domain.MaxLen("name", name, 255); err != nil {
		return domain.Plan{}, err
	}
	if err := domain.ValidateCurrency(currency); err != nil {
		return domain.Plan{}, err
	}
	if input.BillingInterval != domain.BillingMonthly && input.BillingInterval != domain.BillingYearly {
		return domain.Plan{}, errs.Invalid("billing_interval", "must be monthly or yearly")
	}
	if input.BaseAmountCents < 0 {
		return domain.Plan{}, errs.Invalid("base_amount_cents", "base fee must be 0 or more")
	}
	// Create always makes the plan active. A caller sending another status
	// used to get a 201 that ignored it.
	if input.Status != "" && domain.PlanStatus(input.Status) != domain.PlanActive {
		return domain.Plan{}, errs.Invalid("status", "a plan is created active; archive it with PATCH")
	}
	timing, err := baseBillTiming(input.BaseAmountCents, input.BaseBillTiming)
	if err != nil {
		return domain.Plan{}, err
	}
	taxCode := strings.TrimSpace(input.TaxCode)
	if err := domain.ValidateStripeTaxCode("tax_code", taxCode); err != nil {
		return domain.Plan{}, err
	}
	return domain.Plan{
		Code:            code,
		Name:            name,
		Description:     strings.TrimSpace(input.Description),
		Currency:        currency,
		BillingInterval: input.BillingInterval,
		Status:          domain.PlanActive,
		BaseAmountCents: input.BaseAmountCents,
		BaseBillTiming:  timing,
		TaxCode:         taxCode,
	}, nil
}

// baseBillTiming applies ADR-130 §14 to a plan's base fee and billing
// timing. The timing is required when the base fee is above 0: peers default
// it both ways, so the plan states it. With a base of 0 it may be left out
// and is in_arrears. A base of 0 billed in_advance is refused: an in_advance
// plan moves the cycle invoice's header to the next period even at base 0,
// so a usage-only plan must be in_arrears.
func baseBillTiming(base int64, timing domain.BillTiming) (domain.BillTiming, error) {
	if timing != "" && !timing.IsValid() {
		return "", errs.Invalid("base_bill_timing", "must be in_advance or in_arrears")
	}
	switch {
	case base > 0 && timing == "":
		return "", errs.Invalid("base_bill_timing", "is required when base_amount_cents is above 0: in_advance bills the base fee at the start of each period, in_arrears at the end")
	case base == 0 && timing == domain.BillInAdvance:
		return "", errs.Invalid("base_bill_timing", "must be in_arrears when base_amount_cents is 0: a plan with no base fee bills its usage at the end of each period")
	case timing == "":
		return domain.BillInArrears, nil
	}
	return timing, nil
}

func (s *Service) GetPlan(ctx context.Context, tenantID, id string) (domain.Plan, error) {
	return s.store.GetPlan(ctx, tenantID, id)
}

// LiveSubscriptionCount is how many subscriptions keep the plan's base fee
// fixed (ADR-034): every status but canceled. The plan page reads it to
// disable the base fee input the server would refuse, so the page and the
// refusal agree on one number. Zero when no subscription reader is wired.
func (s *Service) LiveSubscriptionCount(ctx context.Context, tenantID, planID string) (int, error) {
	if s.subPlanUsage == nil {
		return 0, nil
	}
	return s.subPlanUsage.CountLiveSubsByPlan(ctx, tenantID, planID)
}

// PlanUsed reports whether any subscription ever used the plan: then a price
// write applies from each subscriber's next period (ADR-130 §5).
func (s *Service) PlanUsed(ctx context.Context, tenantID, planID string) (bool, error) {
	return s.store.PlanUsed(ctx, tenantID, planID)
}

func (s *Service) ListPlans(ctx context.Context, tenantID string) ([]domain.Plan, error) {
	return s.store.ListPlans(ctx, tenantID)
}

// UpdatePlanInput is the PATCH /v1/plans/{id} body. A field left out keeps
// its stored value. Description, TaxCode and BaseAmountCents are pointers
// because an empty string or 0 is a real value for them, so "left out" has
// to be told apart from "set to empty". code, currency and billing_interval
// cannot change, so they are not here.
type UpdatePlanInput struct {
	Name            string            `json:"name,omitempty"`
	Description     *string           `json:"description,omitempty"`
	BaseAmountCents *int64            `json:"base_amount_cents,omitempty"`
	BaseBillTiming  domain.BillTiming `json:"base_bill_timing,omitempty"`
	Status          string            `json:"status,omitempty"`
	TaxCode         *string           `json:"tax_code,omitempty"`
}

// UpdatePlan patches a plan (ADR-130 §5):
//   - name, description, tax_code and status change at once;
//   - the base fee is fixed while a live subscription uses the plan
//     (ADR-034; it becomes a next-period edit if PR-7 ships);
//   - base_bill_timing is fixed once the plan is used;
//   - in_advance with a base of 0 is refused (ADR-130 §14), so a used
//     in_advance plan keeps a base fee above 0.
func (s *Service) UpdatePlan(ctx context.Context, tenantID, id string, input UpdatePlanInput) (domain.Plan, error) {
	if input.BaseBillTiming != "" && !input.BaseBillTiming.IsValid() {
		return domain.Plan{}, errs.Invalid("base_bill_timing", "must be in_advance or in_arrears")
	}
	if input.BaseAmountCents != nil && *input.BaseAmountCents < 0 {
		return domain.Plan{}, errs.Invalid("base_amount_cents", "base fee must be 0 or more")
	}
	if input.Status != "" {
		// Checked here, not left to plans_status_check: the constraint's
		// error is a 500, which the idempotency middleware caches.
		switch domain.PlanStatus(input.Status) {
		case domain.PlanActive, domain.PlanArchived:
		default:
			return domain.Plan{}, errs.Invalid("status", "must be active or archived")
		}
	}
	var taxCode string
	if input.TaxCode != nil {
		taxCode = strings.TrimSpace(*input.TaxCode)
		if err := domain.ValidateStripeTaxCode("tax_code", taxCode); err != nil {
			return domain.Plan{}, err
		}
	}

	existing, err := s.store.GetPlan(ctx, tenantID, id)
	if err != nil {
		return domain.Plan{}, err
	}
	// Base fee lock (ADR-034): a change to it would change what every live
	// subscriber is billed at their next close. Counted before the write;
	// the guard below re-checks the fee it locked against the one counted.
	baseChanged := input.BaseAmountCents != nil && *input.BaseAmountCents != existing.BaseAmountCents
	if baseChanged && s.subPlanUsage != nil {
		count, err := s.subPlanUsage.CountLiveSubsByPlan(ctx, tenantID, existing.ID)
		if err != nil {
			return domain.Plan{}, fmt.Errorf("check sub usage: %w", err)
		}
		if count > 0 {
			return domain.Plan{}, errs.Invalid("plan", planInUseMessage(count))
		}
	}

	return s.store.UpdatePlan(ctx, tenantID, id, func(p domain.Plan, used bool) (domain.Plan, error) {
		if p.BaseAmountCents != existing.BaseAmountCents {
			return domain.Plan{}, errs.InvalidState("This plan changed while the request was running. Refresh the page and try again.")
		}
		if name := strings.TrimSpace(input.Name); name != "" {
			p.Name = name
		}
		if input.Description != nil {
			p.Description = strings.TrimSpace(*input.Description)
		}
		if input.BaseAmountCents != nil {
			p.BaseAmountCents = *input.BaseAmountCents
		}
		if input.BaseBillTiming != "" && input.BaseBillTiming != p.BaseBillTiming {
			if used {
				return domain.Plan{}, errs.Invalid("base_bill_timing", "A subscription has used this plan, so its billing timing is fixed. Create a new plan with the other timing instead.")
			}
			p.BaseBillTiming = input.BaseBillTiming
		}
		if p.BaseAmountCents == 0 && p.BaseBillTiming == domain.BillInAdvance {
			field := "base_bill_timing"
			if input.BaseAmountCents != nil {
				field = "base_amount_cents"
			}
			// A used plan's timing is fixed (above), so advice to change it
			// would be refused too.
			if used {
				return domain.Plan{}, errs.Invalid(field, "This plan bills its base fee in_advance, and a subscription has used it, so the base fee must stay above 0. To charge no base fee, create a new plan that bills in_arrears.")
			}
			return domain.Plan{}, errs.Invalid(field, "A plan with no base fee must bill in_arrears: set base_bill_timing to in_arrears, or keep a base fee above 0.")
		}
		if input.Status != "" {
			p.Status = domain.PlanStatus(input.Status)
		}
		if input.TaxCode != nil {
			p.TaxCode = taxCode
		}
		return p, nil
	})
}

// planInUseMessage is the refusal for a base fee change on a plan that live
// subscriptions use (ADR-034). It names the count, not the field: the caller
// knows what it sent.
func planInUseMessage(liveSubs int) string {
	return fmt.Sprintf("%s this plan, so its base fee is fixed. "+
		"Create a new plan with the new fee instead.", subscriptionsUse(liveSubs))
}

// subscriptionsUse reads "1 subscription uses" or "3 subscriptions use".
func subscriptionsUse(n int) string {
	if n == 1 {
		return "1 subscription uses"
	}
	return fmt.Sprintf("%d subscriptions use", n)
}

// ErrNotFoundAs maps a store not-found to the named resource's 404.
func notFoundAs(err error, resource string) error {
	if errors.Is(err, errs.ErrNotFound) {
		return errs.NotFound(resource)
	}
	return err
}
