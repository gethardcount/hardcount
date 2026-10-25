package usage

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/subscription"
)

// CustomerLookup is the narrow surface CustomerUsageService needs from
// customer.Store. Returns errs.ErrNotFound for cross-tenant IDs (RLS hides
// the row); the handler propagates that as 404 customer_not_found.
type CustomerLookup interface {
	Get(ctx context.Context, tenantID, id string) (domain.Customer, error)
}

// SubscriptionLister returns the customer's subscriptions hydrated with
// their items so we can collect the meter union across subscribed plans.
type SubscriptionLister interface {
	List(ctx context.Context, filter subscription.ListFilter) ([]domain.Subscription, int, error)
}

// PricingReader reads the plans and meters the response names.
type PricingReader interface {
	GetPlan(ctx context.Context, tenantID, id string) (domain.Plan, error)
	GetMeter(ctx context.Context, tenantID, id string) (domain.Meter, error)
}

// UsageRater prices a subscription's usage the way billing does (ADR-130
// §10). Implemented by *billing.Engine, wired in router.go: the rater lives
// in billing, so the customer page prices usage the way the invoice does.
type UsageRater interface {
	// RateCurrentPeriod is the subscription's current period exactly as its
	// close would bill it now.
	RateCurrentPeriod(ctx context.Context, sub domain.Subscription) (domain.RatedSubscription, error)
	// RateWindow is an estimate over [from, to), priced at the
	// subscription's current-period pin.
	RateWindow(ctx context.Context, sub domain.Subscription, from, to time.Time) (domain.RatedSubscription, error)
}

// CustomerUsageService composes the per-domain reads behind GET
// /v1/customers/{id}/usage: customer existence, subscription set, period
// resolution, the rater's buckets, totals and warnings.
type CustomerUsageService struct {
	usage         *Service
	customers     CustomerLookup
	subscriptions SubscriptionLister
	pricing       PricingReader
	rater         UsageRater
}

// NewCustomerUsageService wires the read-side composition. Every
// collaborator is required, the rater through SetRater.
func NewCustomerUsageService(
	usageSvc *Service,
	customers CustomerLookup,
	subscriptions SubscriptionLister,
	pricing PricingReader,
) *CustomerUsageService {
	return &CustomerUsageService{
		usage:         usageSvc,
		customers:     customers,
		subscriptions: subscriptions,
		pricing:       pricing,
	}
}

// SetRater wires the billing rater. The engine is built after this service,
// so it is set after construction.
func (s *CustomerUsageService) SetRater(r UsageRater) { s.rater = r }

// CustomerUsagePeriod is the input window. Both From and To zero → default
// to the customer's current billing cycle. Partial bounds (one zero, one
// non-zero) are rejected with a 422 by resolvePeriod.
type CustomerUsagePeriod struct {
	From time.Time
	To   time.Time
}

// CustomerUsageResult is the response shape for GET /v1/customers/{id}/usage.
// Snake-case JSON keys, struct-tag enforced. Slices default to non-nil so
// the wire emits "[]" not "null" — clients can iterate without null guards.
type CustomerUsageResult struct {
	CustomerID    string                      `json:"customer_id"`
	Period        CustomerUsagePeriodOut      `json:"period"`
	Subscriptions []CustomerUsageSubscription `json:"subscriptions"`
	Meters        []CustomerUsageMeter        `json:"meters"`
	Totals        []CustomerUsageTotal        `json:"totals"`
	Warnings      []string                    `json:"warnings"`
	// Buckets is the daily-grain time series powering the customer-usage
	// chart. One entry per UTC day in [period.from, period.to), missing
	// days zero-filled so chart consumers don't have to gap-fill. Each
	// bucket carries per-meter quantities — the frontend stacks them or
	// flattens depending on the meter cardinality. Bucket quantities are raw
	// SUM(quantity) per meter and include usage no price matched, so they
	// need not add up to Meters[] totals: those use each meter's
	// aggregation (max, last, …) and leave the no_price row out.
	Buckets []CustomerUsageBucket `json:"buckets"`
}

// CustomerUsagePeriodOut tells the client which window the response covers
// and whether the server inferred it from the current cycle or honored an
// explicit ?from=&to=.
type CustomerUsagePeriodOut struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Source string    `json:"source"`
}

// CustomerUsageSubscription summarises one of the customer's subscriptions
// that overlapped the queried window. Plan info is denormalised so the
// dashboard renders "Plan: AI API Pro · cycle Apr 1 → May 1" without a
// follow-up call.
type CustomerUsageSubscription struct {
	ID                 string    `json:"id"`
	PlanID             string    `json:"plan_id"`
	PlanName           string    `json:"plan_name"`
	Currency           string    `json:"currency"`
	CurrentPeriodStart time.Time `json:"current_period_start"`
	CurrentPeriodEnd   time.Time `json:"current_period_end"`
}

// CustomerUsageMeter is one meter the customer's plans price, with its
// rolled-up usage and cost over the queried window. Prices carries one row
// per price that took usage, with its plan: two plans can price one meter
// differently (ADR-130 §10).
type CustomerUsageMeter struct {
	MeterID          string               `json:"meter_id"`
	EventName        string               `json:"event_name"`
	MeterName        string               `json:"meter_name"`
	Unit             string               `json:"unit"`
	Currency         string               `json:"currency"`
	TotalQuantity    decimal.Decimal      `json:"total_quantity"`
	TotalAmountCents int64                `json:"total_amount_cents"`
	Prices           []CustomerUsagePrice `json:"prices"`
}

// CustomerUsagePrice is the usage one plan price took over the window, and
// what it bills. DimensionMatch is the events the price takes; a catch-all
// price has none.
type CustomerUsagePrice struct {
	PriceID        string            `json:"price_id,omitempty"`
	PlanID         string            `json:"plan_id"`
	DimensionMatch map[string]string `json:"dimension_match,omitempty"`
	Quantity       decimal.Decimal   `json:"quantity"`
	AmountCents    int64             `json:"amount_cents"`
	// UnitAmountDecimal is the per-unit price to display (decimal cents): the
	// configured rate for per_unit, else the effective amount÷qty — the SAME
	// value the invoice line shows (ADR-054, via
	// domain.DisplayUnitAmountDecimalFor).
	UnitAmountDecimal *string `json:"unit_amount_decimal,omitempty"`
	// NoPrice marks the usage no price of the plan took: billing bills
	// nothing for it. Surfaced as a row so a mislabeled dimension value (the
	// most likely integration error on a matrix-priced meter) is visible to
	// the operator. Excluded from the meter's totals (they mirror the
	// invoice) and from the public cost dashboard.
	NoPrice bool `json:"no_price,omitempty"`
}

// CustomerUsageTotal is one currency's roll-up across all meters. We
// always emit a list (one entry per distinct currency) even when there's
// only one currency — consistent shape lets clients read totals[0] without
// branching, and lines up with /v1/* "always lists" convention.
type CustomerUsageTotal struct {
	Currency    string `json:"currency"`
	AmountCents int64  `json:"amount_cents"`
}

// CustomerUsageBucket is one UTC-day cell of the time-series. PerMeter
// is keyed by meter_id with the day's total quantity (decimal so the
// NUMERIC(38,12) storage precision round-trips). Days with no events
// are still included with PerMeter empty / zeroed so the chart renders
// continuous time without client-side gap-filling.
type CustomerUsageBucket struct {
	BucketStart time.Time                  `json:"bucket_start"`
	PerMeter    map[string]decimal.Decimal `json:"per_meter"`
}

// Get composes the customer-usage view (ADR-130 §10). Order:
//  1. Customer existence (RLS makes cross-tenant IDs return ErrNotFound).
//  2. Active+trialing subscriptions for the customer.
//  3. Period resolution (current cycle from primary sub, or explicit
//     ?from=&to= validated for partial bounds, ordering, 1-year cap).
//  4. Each subscription through billing's rater: with no window, its own
//     current period exactly as its close would bill it now (windows,
//     floors, threshold watermark, ladder); with an explicit window, an
//     estimate at its current-period prices.
//  5. Per-currency totals roll-up, warnings collection, subscription
//     summary.
func (s *CustomerUsageService) Get(ctx context.Context, tenantID, customerID string, period CustomerUsagePeriod) (CustomerUsageResult, error) {
	customerID = strings.TrimSpace(customerID)
	if customerID == "" {
		return CustomerUsageResult{}, errs.Required("customer_id")
	}
	if s.rater == nil {
		return CustomerUsageResult{}, fmt.Errorf("customer usage: rater not wired")
	}

	if _, err := s.customers.Get(ctx, tenantID, customerID); err != nil {
		return CustomerUsageResult{}, err
	}

	subs, _, err := s.subscriptions.List(ctx, subscription.ListFilter{
		TenantID:   tenantID,
		CustomerID: customerID,
	})
	if err != nil {
		return CustomerUsageResult{}, fmt.Errorf("list subscriptions: %w", err)
	}
	activeSubs := filterActiveSubs(subs)

	from, to, source, err := resolvePeriod(period, activeSubs, func() time.Time {
		return s.usage.effectiveNow(ctx, tenantID, customerID)
	})
	if err != nil {
		return CustomerUsageResult{}, err
	}

	plansByID, err := s.readPlans(ctx, tenantID, activeSubs)
	if err != nil {
		return CustomerUsageResult{}, err
	}

	var (
		warnings []string
		rows     []domain.RatedUsage
		meterIDs []string
		seen     = map[string]bool{}
	)
	for _, sub := range activeSubs {
		var rated domain.RatedSubscription
		if source == "explicit" {
			rated, err = s.rater.RateWindow(ctx, sub, from, to)
		} else {
			rated, err = s.rater.RateCurrentPeriod(ctx, sub)
		}
		if err != nil {
			return CustomerUsageResult{}, fmt.Errorf("rate subscription %s: %w", sub.ID, err)
		}
		rows = append(rows, rated.Usage...)
		for _, id := range rated.MeterIDs {
			if !seen[id] {
				seen[id] = true
				meterIDs = append(meterIDs, id)
			}
		}
	}
	if source == "explicit" && len(activeSubs) > 0 {
		warnings = append(warnings, "Estimate: usage over a chosen window is priced at each subscription's current-period prices.")
	}

	meters, w, err := s.composeMeters(ctx, tenantID, meterIDs, rows, plansByID)
	if err != nil {
		return CustomerUsageResult{}, err
	}
	warnings = append(warnings, w...)

	subSummaries := buildSubscriptionSummaries(activeSubs, plansByID)
	totals := computeTotals(meters)

	// Daily time series for the chart. Cheap relative to the rating reads —
	// single SQL pass with date_trunc + sum. On store error we degrade to an
	// empty buckets slice rather than failing the whole response: the chart
	// vanishes, the rest stays.
	buckets := []CustomerUsageBucket{}
	if len(meterIDs) > 0 {
		dailyRows, err := s.usage.AggregateDailyBuckets(ctx, tenantID, customerID, meterIDs, from, to)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("daily bucket aggregation failed: %v", err))
		} else {
			buckets = composeDailyBuckets(dailyRows, from, to)
		}
	}

	return CustomerUsageResult{
		CustomerID:    customerID,
		Period:        CustomerUsagePeriodOut{From: from, To: to, Source: source},
		Subscriptions: subSummaries,
		Meters:        meters,
		Totals:        totals,
		Warnings:      nilToEmptyStrings(warnings),
		Buckets:       buckets,
	}, nil
}

// composeDailyBuckets fills missing UTC-day buckets in [from, to) with
// empty PerMeter maps so the chart consumer gets continuous time.
// Bucket boundaries are date_trunc('day', from) inclusive through
// date_trunc('day', to) exclusive — same alignment the SQL produces.
//
// Non-empty buckets are sorted by date by virtue of the gap-fill walk;
// per-meter map insertion is order-irrelevant (frontend keys by id).
func composeDailyBuckets(rows []DailyBucketRow, from, to time.Time) []CustomerUsageBucket {
	// Index rows by truncated UTC day for O(1) lookup during the walk.
	byDay := make(map[time.Time][]DailyBucketRow, len(rows))
	for _, r := range rows {
		day := r.BucketStart.UTC().Truncate(24 * time.Hour)
		byDay[day] = append(byDay[day], r)
	}

	// Walk inclusive-from through exclusive-to one day at a time.
	start := from.UTC().Truncate(24 * time.Hour)
	end := to.UTC().Truncate(24 * time.Hour)
	if !to.UTC().Equal(end) {
		end = end.Add(24 * time.Hour) // include the partial trailing day
	}

	out := make([]CustomerUsageBucket, 0, int(end.Sub(start).Hours()/24)+1)
	for cursor := start; cursor.Before(end); cursor = cursor.Add(24 * time.Hour) {
		bucket := CustomerUsageBucket{BucketStart: cursor, PerMeter: map[string]decimal.Decimal{}}
		for _, r := range byDay[cursor] {
			bucket.PerMeter[r.MeterID] = r.Quantity
		}
		out = append(out, bucket)
	}
	return out
}

// resolvePeriod decides the [from, to) window to query. Default is the
// primary active subscription's current cycle. Explicit ?from=&to= must
// have from < to and span at most one calendar year (to ≤ from + 1y).
//
// Returns a DomainError so the handler maps to 422 with the right code.
//
// now is the customer's effective now (test-clock frozen_time when pinned).
// It is read only for the default, to skip cycles that have not started.
func resolvePeriod(period CustomerUsagePeriod, subs []domain.Subscription, now func() time.Time) (time.Time, time.Time, string, error) {
	hasFrom := !period.From.IsZero()
	hasTo := !period.To.IsZero()

	if hasFrom != hasTo {
		return time.Time{}, time.Time{}, "", errs.Invalid("period", "both from and to must be supplied together")
	}

	if hasFrom && hasTo {
		if !period.From.Before(period.To) {
			return time.Time{}, time.Time{}, "", errs.Invalid("period", "from must be strictly before to")
		}
		// Explicit windows are capped at one CALENDAR year (from + 1y), so
		// the LATERAL JOIN cost stays bounded — see
		// docs/dev/design/customer-usage.md. Calendar, not 365×24h: a leap
		// year (2028-01-01..2028-12-31) and a 366-day yearly billing cycle
		// are both one year and must be accepted.
		if period.To.After(period.From.AddDate(1, 0, 0)) {
			return time.Time{}, time.Time{}, "", errs.Invalid("period", "window must be ≤ 1 year")
		}
		return period.From, period.To, "explicit", nil
	}

	// Default: pick the primary subscription — the one whose cycle started
	// last. A cycle that starts after now has not started: a trialing sub
	// stores its FIRST PAID period (firstPeriodAfterTrial), which begins at
	// trial end. Picking it while another sub's cycle is running rated every
	// meter over a future window, so the running cycle's usage read $0.
	// Only when NO cycle has started (e.g. the customer's only sub is
	// trialing) do we fall back to the latest-start pick across all subs,
	// which is the long-standing behaviour for that case.
	n := now()
	var primary, primaryAny *domain.Subscription
	for i := range subs {
		sub := &subs[i]
		if sub.CurrentBillingPeriodStart == nil || sub.CurrentBillingPeriodEnd == nil {
			continue
		}
		if primaryAny == nil || sub.CurrentBillingPeriodStart.After(*primaryAny.CurrentBillingPeriodStart) {
			primaryAny = sub
		}
		if sub.CurrentBillingPeriodStart.After(n) {
			continue
		}
		if primary == nil || sub.CurrentBillingPeriodStart.After(*primary.CurrentBillingPeriodStart) {
			primary = sub
		}
	}
	if primary == nil {
		primary = primaryAny
	}
	if primary == nil {
		return time.Time{}, time.Time{}, "", errs.Invalid(
			"period",
			"customer has no active subscription with a current billing cycle; pass ?from= and ?to= to query usage outside a billing cycle",
		).WithCode("customer_has_no_subscription")
	}
	return *primary.CurrentBillingPeriodStart, *primary.CurrentBillingPeriodEnd, "current_billing_cycle", nil
}

// readPlans reads each distinct plan of the subscriptions' items once.
func (s *CustomerUsageService) readPlans(ctx context.Context, tenantID string, subs []domain.Subscription) (map[string]domain.Plan, error) {
	plans := map[string]domain.Plan{}
	for _, sub := range subs {
		for _, item := range sub.Items {
			if _, ok := plans[item.PlanID]; ok {
				continue
			}
			plan, err := s.pricing.GetPlan(ctx, tenantID, item.PlanID)
			if err != nil {
				return nil, fmt.Errorf("get plan %q: %w", item.PlanID, err)
			}
			plans[plan.ID] = plan
		}
	}
	return plans, nil
}

// composeMeters groups the rated rows by meter, in meterIDs order, one row
// per (plan, price) with the windows of a price added together. A bucket
// below zero counts as 0, as on the invoice, with a warning; usage no price
// took is a no_price row outside the totals, with a warning.
func (s *CustomerUsageService) composeMeters(ctx context.Context, tenantID string, meterIDs []string, rows []domain.RatedUsage, plans map[string]domain.Plan) ([]CustomerUsageMeter, []string, error) {
	var warnings []string
	byMeter := map[string][]domain.RatedUsage{}
	for _, r := range rows {
		byMeter[r.MeterID] = append(byMeter[r.MeterID], r)
	}
	out := make([]CustomerUsageMeter, 0, len(meterIDs))
	for _, meterID := range meterIDs {
		meter, err := s.pricing.GetMeter(ctx, tenantID, meterID)
		if err != nil {
			return nil, nil, fmt.Errorf("get meter %q: %w", meterID, err)
		}
		m := CustomerUsageMeter{
			MeterID:   meter.ID,
			EventName: meter.EventName,
			MeterName: meter.Name,
			Unit:      meter.Unit,
			Prices:    []CustomerUsagePrice{},
		}
		type key struct{ plan, price string }
		index := map[key]int{}
		for _, r := range byMeter[meterID] {
			if r.BelowZero {
				warnings = append(warnings, fmt.Sprintf("meter %q: usage below zero (%s) after corrections — counted as 0, as on the invoice", meter.EventName, r.Quantity.String()))
				continue
			}
			if m.Currency == "" {
				m.Currency = plans[r.PlanID].Currency
			}
			k := key{plan: r.PlanID, price: r.PriceID}
			i, ok := index[k]
			if !ok {
				i = len(m.Prices)
				index[k] = i
				m.Prices = append(m.Prices, CustomerUsagePrice{
					PriceID: r.PriceID, PlanID: r.PlanID, DimensionMatch: r.DimensionMatch, NoPrice: r.PriceID == "",
				})
			}
			p := &m.Prices[i]
			p.Quantity = p.Quantity.Add(r.Quantity)
			p.AmountCents += r.AmountCents
			if r.PriceID == "" {
				continue
			}
			m.TotalQuantity = m.TotalQuantity.Add(r.Quantity)
			m.TotalAmountCents += r.AmountCents
		}
		for i := range m.Prices {
			p := &m.Prices[i]
			if p.NoPrice {
				warnings = append(warnings, fmt.Sprintf("meter %q has usage that matches no price of plan %q — not billed (see the no_price row)", meter.EventName, plans[p.PlanID].Name))
				continue
			}
			unit := unitAmountOf(byMeter[meterID], p.PlanID, p.PriceID, p.AmountCents, p.Quantity)
			p.UnitAmountDecimal = &unit
		}
		out = append(out, m)
	}
	return out, warnings, nil
}

// unitAmountOf is the display rate of one price row: the rate the rater
// stamped when every window of it agrees (a per_unit price's configured
// rate), else the effective amount over the quantity.
func unitAmountOf(rows []domain.RatedUsage, planID, priceID string, amount int64, qty decimal.Decimal) string {
	var rate *decimal.Decimal
	for _, r := range rows {
		if r.PlanID != planID || r.PriceID != priceID || r.BelowZero {
			continue
		}
		if rate == nil {
			v := r.UnitAmountDecimal
			rate = &v
			continue
		}
		if !rate.Equal(r.UnitAmountDecimal) {
			return domain.EffectiveUnitAmountDecimalFor(amount, qty).String()
		}
	}
	if rate == nil {
		return domain.EffectiveUnitAmountDecimalFor(amount, qty).String()
	}
	return rate.String()
}

// filterActiveSubs returns only active and trialing subs. Paused / canceled
// / draft / archived subs don't contribute to "what is this customer using
// right now" — their events are still queryable via /v1/usage-events with
// an explicit window.
func filterActiveSubs(subs []domain.Subscription) []domain.Subscription {
	out := make([]domain.Subscription, 0, len(subs))
	for _, sub := range subs {
		if sub.Status == domain.SubscriptionActive || sub.Status == domain.SubscriptionTrialing {
			out = append(out, sub)
		}
	}
	return out
}

// buildSubscriptionSummaries flattens (sub, item) into one summary entry
// per item. A multi-item subscription emits one entry per priced line so
// the dashboard can render "Plan A + Plan B" correctly.
func buildSubscriptionSummaries(subs []domain.Subscription, plansByID map[string]domain.Plan) []CustomerUsageSubscription {
	out := make([]CustomerUsageSubscription, 0, len(subs))
	for _, sub := range subs {
		for _, item := range sub.Items {
			plan, ok := plansByID[item.PlanID]
			if !ok {
				continue
			}
			start, end := time.Time{}, time.Time{}
			if sub.CurrentBillingPeriodStart != nil {
				start = *sub.CurrentBillingPeriodStart
			}
			if sub.CurrentBillingPeriodEnd != nil {
				end = *sub.CurrentBillingPeriodEnd
			}
			out = append(out, CustomerUsageSubscription{
				ID:                 sub.ID,
				PlanID:             plan.ID,
				PlanName:           plan.Name,
				Currency:           plan.Currency,
				CurrentPeriodStart: start,
				CurrentPeriodEnd:   end,
			})
		}
	}
	return out
}

// computeTotals groups each meter's amount by its currency, the plan's,
// and emits one bucket per distinct currency. Order is insertion-stable
// (first currency seen wins position 0) so the response is deterministic
// across calls.
func computeTotals(meters []CustomerUsageMeter) []CustomerUsageTotal {
	bucket := map[string]int64{}
	var order []string
	for _, m := range meters {
		if m.Currency == "" {
			continue
		}
		if _, ok := bucket[m.Currency]; !ok {
			order = append(order, m.Currency)
		}
		bucket[m.Currency] += m.TotalAmountCents
	}
	out := make([]CustomerUsageTotal, 0, len(order))
	for _, cur := range order {
		out = append(out, CustomerUsageTotal{Currency: cur, AmountCents: bucket[cur]})
	}
	return out
}

func nilToEmptyStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
