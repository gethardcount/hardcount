package usage

import (
	"context"
	"errors"
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

// PricingReader resolves plan / meter / rating-rule references when
// assembling the response. Mirrors the surface pricing.Service exposes —
// listed here so the customer-usage code owns no cross-domain state.
type PricingReader interface {
	GetPlan(ctx context.Context, tenantID, id string) (domain.Plan, error)
	GetMeter(ctx context.Context, tenantID, id string) (domain.Meter, error)
	GetRatingRule(ctx context.Context, tenantID, id string) (domain.RatingRuleVersion, error)
	// GetRuleByKeyAsOf + GetOverrideByKeyAsOf: the ADR-070 resolution
	// pair. The customer-usage view must price with the SAME rule the
	// cycle close will bill — version and override in force at the
	// period open — or the running-spend number is wrong for exactly
	// the negotiated-rate customers the spend-cap wedge targets.
	GetRuleByKeyAsOf(ctx context.Context, tenantID, ruleKey string, asOf time.Time) (domain.RatingRuleVersion, error)
	GetOverrideByKeyAsOf(ctx context.Context, tenantID, customerID, ruleKey string, asOf time.Time) (domain.CustomerPriceOverride, error)
	ListMeterPricingRulesByMeter(ctx context.Context, tenantID, meterID string) ([]domain.MeterPricingRule, error)
}

// CustomerUsageService composes the per-domain reads behind GET
// /v1/customers/{id}/usage. See docs/design-customer-usage.md.
//
// The hard work — dimension-match-aware aggregation in
// usage.AggregateByPricingRules — already exists. This service is a
// composition: customer existence, subscription set, period resolution,
// per-meter rating, totals roll-up, warnings collection.
type CustomerUsageService struct {
	usage         *Service
	customers     CustomerLookup
	subscriptions SubscriptionLister
	pricing       PricingReader
}

// NewCustomerUsageService wires the read-side composition. All four
// collaborators are required.
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
	// SUM(quantity) per meter and include usage no pricing rule matched, so
	// they need not add up to Meters[] totals: those use each meter's
	// aggregation (max, last, …) and leave the unmatched row out.
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

// CustomerUsageMeter is one meter on the customer's plan(s) with its
// rolled-up usage and cost over the queried window. Rules carries the
// per-rule breakdown for multi-dim meters; for a flat single-rule meter
// the slice is length 1 with no DimensionMatch.
type CustomerUsageMeter struct {
	MeterID          string              `json:"meter_id"`
	MeterKey         string              `json:"meter_key"`
	MeterName        string              `json:"meter_name"`
	Unit             string              `json:"unit"`
	Currency         string              `json:"currency"`
	TotalQuantity    decimal.Decimal     `json:"total_quantity"`
	TotalAmountCents int64               `json:"total_amount_cents"`
	Rules            []CustomerUsageRule `json:"rules"`
}

// CustomerUsageRule is one row of the priority+claim resolution: events
// claimed by a single pricing rule, rolled up into a quantity and rated
// through pricing.ComputeAmountCents. DimensionMatch echoes the meter
// pricing rule's match expression (the canonical pricing identity).
type CustomerUsageRule struct {
	RatingRuleVersionID string          `json:"rating_rule_version_id"`
	RuleKey             string          `json:"rule_key"`
	DimensionMatch      map[string]any  `json:"dimension_match,omitempty"`
	Quantity            decimal.Decimal `json:"quantity"`
	AmountCents         int64           `json:"amount_cents"`
	// UnitAmountDecimal is the per-unit price to display (decimal cents): the
	// configured nominal rate for flat rules, else the effective amount÷qty —
	// the SAME value the invoice line shows (ADR-054, via
	// domain.DisplayUnitAmountDecimalFor). Rendered with the decimal-aware rate
	// formatter so a sub-cent rate never collapses to $0.00.
	UnitAmountDecimal *string `json:"unit_amount_decimal,omitempty"`
	// Unmatched marks the bucket of events that matched NO pricing rule
	// on a meter with no default binding — usage the engine bills
	// NOTHING for (same resolution order as the cycle). Surfaced as a
	// first-class row so a mislabeled dimension value (the most likely
	// integration error on a matrix-priced meter) is visible to the
	// operator instead of leaking revenue behind a server-log WARN.
	// Excluded from the meter's totals (they mirror the invoice) and
	// filtered out of the public cost-dashboard projection —
	// pricing-config problems are operator information.
	Unmatched bool `json:"unmatched,omitempty"`
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

// Get composes the customer-usage view. Order:
//  1. Customer existence (RLS makes cross-tenant IDs return ErrNotFound).
//  2. Active+trialing subscriptions for the customer.
//  3. Period resolution (current cycle from primary sub, or explicit
//     ?from=&to= validated for partial bounds, ordering, 1-year cap).
//  4. Walk the meter union across subscribed plans, calling
//     usage.AggregateByPricingRules per meter then ComputeAmountCents per
//     rule. Same code path the cycle scan uses → dashboard math == invoice
//     math.
//  5. Per-currency totals roll-up, warnings collection, subscription
//     summary.
func (s *CustomerUsageService) Get(ctx context.Context, tenantID, customerID string, period CustomerUsagePeriod) (CustomerUsageResult, error) {
	customerID = strings.TrimSpace(customerID)
	if customerID == "" {
		return CustomerUsageResult{}, errs.Required("customer_id")
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

	meterIDs, plansByID, err := s.collectMeters(ctx, tenantID, activeSubs)
	if err != nil {
		return CustomerUsageResult{}, err
	}

	var warnings []string
	meters := make([]CustomerUsageMeter, 0, len(meterIDs))
	for _, meterID := range meterIDs {
		m, w, err := s.rateMeter(ctx, tenantID, customerID, meterID, from, to)
		if err != nil {
			return CustomerUsageResult{}, err
		}
		warnings = append(warnings, w...)
		meters = append(meters, m)
	}

	subSummaries := buildSubscriptionSummaries(activeSubs, plansByID)
	totals := computeTotals(meters)

	// Daily time series for the chart. Cheap relative to the per-meter
	// rate calls we already did — single SQL pass with date_trunc + sum.
	// On store error we degrade to an empty buckets slice rather than
	// failing the whole response: the chart vanishes, the rest stays.
	buckets := []CustomerUsageBucket{}
	if len(meterIDs) > 0 {
		rows, err := s.usage.AggregateDailyBuckets(ctx, tenantID, customerID, meterIDs, from, to)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("daily bucket aggregation failed: %v", err))
		} else {
			buckets = composeDailyBuckets(rows, from, to)
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
		// docs/design-customer-usage.md. Calendar, not 365×24h: a leap
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

// collectMeters walks each active subscription's items, fetches each
// distinct plan once (cached), and returns the union of the plans'
// meter IDs in deterministic insertion order so the response is stable
// across calls.
func (s *CustomerUsageService) collectMeters(ctx context.Context, tenantID string, subs []domain.Subscription) ([]string, map[string]domain.Plan, error) {
	plans := map[string]domain.Plan{}
	seen := map[string]struct{}{}
	var meterIDs []string

	for _, sub := range subs {
		for _, item := range sub.Items {
			if _, ok := plans[item.PlanID]; !ok {
				plan, err := s.pricing.GetPlan(ctx, tenantID, item.PlanID)
				if err != nil {
					return nil, nil, fmt.Errorf("get plan %q: %w", item.PlanID, err)
				}
				plans[plan.ID] = plan
			}
			for _, meterID := range plans[item.PlanID].MeterIDs {
				if _, ok := seen[meterID]; ok {
					continue
				}
				seen[meterID] = struct{}{}
				meterIDs = append(meterIDs, meterID)
			}
		}
	}
	return meterIDs, plans, nil
}

// rateMeter aggregates one meter's events for the customer over [from, to)
// and rates each per-rule bucket through the pricing engine. The meter's
// reported currency is the first rating rule's currency; mismatches across
// rules surface as warnings (don't fail the request — better to render
// imperfect numbers than nothing while the operator fixes config).
func (s *CustomerUsageService) rateMeter(ctx context.Context, tenantID, customerID, meterID string, from, to time.Time) (CustomerUsageMeter, []string, error) {
	var warnings []string

	meter, err := s.pricing.GetMeter(ctx, tenantID, meterID)
	if err != nil {
		return CustomerUsageMeter{}, nil, fmt.Errorf("get meter %q: %w", meterID, err)
	}

	defaultMode := mapMeterAggregation(meter.Aggregation)
	aggs, err := s.usage.AggregateByPricingRules(ctx, tenantID, customerID, meterID, defaultMode, from, to)
	if err != nil {
		return CustomerUsageMeter{}, nil, err
	}

	rules, err := s.pricing.ListMeterPricingRulesByMeter(ctx, tenantID, meterID)
	if err != nil {
		return CustomerUsageMeter{}, nil, err
	}
	rulesByID := make(map[string]domain.MeterPricingRule, len(rules))
	for _, rule := range rules {
		rulesByID[rule.ID] = rule
	}

	out := CustomerUsageMeter{
		MeterID:   meter.ID,
		MeterKey:  meter.Key,
		MeterName: meter.Name,
		Unit:      meter.Unit,
		Rules:     []CustomerUsageRule{},
	}

	totalQty := decimal.Zero
	var totalCents int64

	for _, agg := range aggs {
		ratingRuleID := agg.RatingRuleVersionID
		if ratingRuleID == "" {
			ratingRuleID = meter.RatingRuleVersionID
		}
		if ratingRuleID == "" {
			// The unclaimed bucket: events matching no pricing rule on a
			// meter with no default binding. The engine bills nothing for
			// these (loud WARN at cycle close) — emit a first-class
			// unmatched row so the operator SEES the unbilled volume where
			// they investigate usage, instead of a dropped warning string.
			// Deliberately outside totals: totals mirror the invoice.
			if !agg.Quantity.IsZero() {
				out.Rules = append(out.Rules, CustomerUsageRule{
					Unmatched: true,
					Quantity:  agg.Quantity,
				})
				warnings = append(warnings, fmt.Sprintf("meter %q has usage matching no pricing rule — not billed (see the unmatched row)", meter.Key))
			}
			continue
		}

		// ADR-070 resolution — mirrors billing's resolveRatedRule so
		// this view matches the invoice: the pinned binding supplies the
		// rule_key; the version in force at the period open prices the
		// window; the customer's override in force at the same instant
		// patches on top. ErrNotFound on the override = list price; any
		// other failure is loud (a silently-list-priced negotiated
		// customer is the audit finding this closes).
		linked, err := s.pricing.GetRatingRule(ctx, tenantID, ratingRuleID)
		if err != nil {
			return CustomerUsageMeter{}, nil, fmt.Errorf("get rating rule %q: %w", ratingRuleID, err)
		}
		ratingRule, err := s.pricing.GetRuleByKeyAsOf(ctx, tenantID, linked.RuleKey, from)
		if err != nil {
			if !errors.Is(err, errs.ErrNotFound) {
				return CustomerUsageMeter{}, nil, fmt.Errorf("resolve rule %q as of period open: %w", linked.RuleKey, err)
			}
			// Key has no active versions (all archived) but the binding
			// still points at one — same fallback the engine applies.
			ratingRule = linked
		}
		if override, oerr := s.pricing.GetOverrideByKeyAsOf(ctx, tenantID, customerID, ratingRule.RuleKey, from); oerr == nil {
			ratingRule = override.ApplyTo(ratingRule)
		} else if !errors.Is(oerr, errs.ErrNotFound) {
			return CustomerUsageMeter{}, nil, fmt.Errorf("resolve override for rule %q: %w", ratingRule.RuleKey, oerr)
		}

		cents, err := domain.ComputeAmountCents(ratingRule, agg.Quantity)
		if err != nil {
			return CustomerUsageMeter{}, nil, fmt.Errorf("rate meter %q rule %q: %w", meter.Key, ratingRuleID, err)
		}

		if out.Currency == "" {
			out.Currency = ratingRule.Currency
		} else if out.Currency != ratingRule.Currency {
			warnings = append(warnings, fmt.Sprintf("meter %q has rating rules with mismatched currencies (%s vs %s)", meter.Key, out.Currency, ratingRule.Currency))
		}

		var dimMatch map[string]any
		if agg.RuleID != "" {
			if rule, ok := rulesByID[agg.RuleID]; ok && len(rule.DimensionMatch) > 0 {
				dimMatch = rule.DimensionMatch
			}
		}

		totalQty = totalQty.Add(agg.Quantity)
		totalCents += cents

		// Same display rate the invoice line shows: nominal for flat rules,
		// effective amount÷qty for tiered — resolved from the same
		// override-applied ratingRule (ADR-054).
		unitAmountDecimal := domain.DisplayUnitAmountDecimalFor(ratingRule, cents, agg.Quantity).String()

		out.Rules = append(out.Rules, CustomerUsageRule{
			RatingRuleVersionID: ratingRule.ID,
			RuleKey:             ratingRule.RuleKey,
			DimensionMatch:      dimMatch,
			Quantity:            agg.Quantity,
			AmountCents:         cents,
			UnitAmountDecimal:   &unitAmountDecimal,
		})
	}

	out.TotalQuantity = totalQty
	out.TotalAmountCents = totalCents
	return out, warnings, nil
}

// mapMeterAggregation translates the meter's stored aggregation string
// ("sum"/"count"/"max"/"last") to the AggregationMode the priority+claim
// resolver accepts. The "last" UI value maps to last_during_period —
// last_ever as a meter default would silently break "current state"
// semantics for unclaimed events (also rejected by the service layer).
func mapMeterAggregation(agg string) domain.AggregationMode {
	switch agg {
	case "sum":
		return domain.AggSum
	case "count":
		return domain.AggCount
	case "max":
		return domain.AggMax
	case "last":
		return domain.AggLastDuringPeriod
	default:
		return domain.AggSum
	}
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

// computeTotals groups each meter's amount by its rating-rule currency
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
