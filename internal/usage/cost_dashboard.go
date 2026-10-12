package usage

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/subscription"
)

// CostDashboardAssembler builds the sanitized projection served by
// GET /v1/public/cost-dashboard/{token}. Two responsibilities:
//   - resolve the public token to a customer (via the customer store)
//   - compose the usage view (via CustomerUsageService) and sanitize
//     it into the public envelope
//
// Lives in the usage package because the heavy lifting is the usage
// math; the customer-package handler imports it via the narrow
// CostDashboardService interface (returns `any` to keep the customer
// package free of usage-domain types).
type CostDashboardAssembler struct {
	customers     CustomerTokenLookup
	usageService  *CustomerUsageService
	subscriptions SubscriptionLister
	credits       CostDashboardCreditReader
}

// CostDashboardCreditReader is the narrow surface the assembler uses to read
// a customer's credit position. The credit service implements it through an
// adapter in internal/api, so this package does not import the credit domain.
type CostDashboardCreditReader interface {
	CreditSummary(ctx context.Context, tenantID, customerID string) (CostDashboardCredits, error)
}

// CustomerTokenLookup is the narrow surface the assembler uses to
// resolve the cost-dashboard token to a customer. RLS-bypass
// implementation lives in customer.PostgresStore.GetByCostDashboardToken
// (the token IS the credential — no tenant context yet).
type CustomerTokenLookup interface {
	GetByCostDashboardToken(ctx context.Context, token string) (domain.Customer, error)
}

// NewCostDashboardAssembler wires the public-projection assembler.
// All four deps are required.
func NewCostDashboardAssembler(customers CustomerTokenLookup, usageSvc *CustomerUsageService, subs SubscriptionLister, credits CostDashboardCreditReader) *CostDashboardAssembler {
	return &CostDashboardAssembler{
		customers:     customers,
		usageService:  usageSvc,
		subscriptions: subs,
		credits:       credits,
	}
}

// CostDashboardProjection is the wire shape returned from
// GET /v1/public/cost-dashboard/{token}. Slice fields default to non-
// nil so the wire emits "[]" not "null" — embed widgets iterate
// without null guards.
//
// What's deliberately ABSENT (sanitization contract):
//   - email, display_name, external_id, metadata (customer PII)
//   - billing_profile (legal name, address, tax_id)
//   - warnings (operator-facing tech messages from CustomerUsageService)
//   - plan_id (internal identifier; plan_name is enough for display)
//   - rating_rule_version_id (internal identifier)
//
// What's PRESENT:
//   - customer_id + tenant_id (caller already has the token, so these
//     are not secrets)
//   - billing_period { start, end, source }
//   - subscriptions (id + plan_name + currency + period only)
//   - usage[] (meter + per-rule breakdown for multi-dim)
//   - totals[] (per-currency rollup)
//   - projected_total_cents (sum across currencies — Stripe-style
//     single-number summary the widget surfaces as the headline)
//   - credits (balance, remaining prepaid commit and promotional credit,
//     and an estimate of what is left after this period's usage so far)
type CostDashboardProjection struct {
	CustomerID          string                      `json:"customer_id"`
	TenantID            string                      `json:"tenant_id"`
	BillingPeriod       CostDashboardPeriod         `json:"billing_period"`
	Subscriptions       []CostDashboardSubscription `json:"subscriptions"`
	Usage               []CostDashboardMeter        `json:"usage"`
	Totals              []CostDashboardTotal        `json:"totals"`
	ProjectedTotalCents int64                       `json:"projected_total_cents"`
	Credits             CostDashboardCredits        `json:"credits"`
	// Livemode marks whether this dashboard reflects real-money usage; the
	// embed shows a "Test mode" banner when false. Shipped in the JSON
	// contract pre-outreach so partner UIs never face a mid-pilot schema
	// addition (ADR-032 consumer-ready JSON).
	Livemode bool `json:"livemode"`
}

// CostDashboardCredits is the customer's credit position (issue #86).
//
// BalanceCents is the ledger balance: credit is drawn down when an invoice is
// issued, so it does not move during a billing period. EstimatedRemainingCents
// is the figure a "live balance" widget wants: the balance minus this period's
// usage so far (projected_total_cents), never below zero. It is an estimate:
// the projection excludes tax, and a customer billed in more than one
// currency has those totals added together.
type CostDashboardCredits struct {
	BalanceCents              int64 `json:"balance_cents"`
	CommitRemainingCents      int64 `json:"commit_remaining_cents"`
	PromotionalRemainingCents int64 `json:"promotional_remaining_cents"`
	EstimatedRemainingCents   int64 `json:"estimated_remaining_cents"`
}

// estimatedRemaining is the credit left after this period's usage so far.
func estimatedRemaining(balanceCents, projectedCents int64) int64 {
	if left := balanceCents - projectedCents; left > 0 {
		return left
	}
	return 0
}

type CostDashboardPeriod struct {
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
	Source string    `json:"source"` // "subscription" | "no_subscription"
}

type CostDashboardSubscription struct {
	ID                 string    `json:"id"`
	PlanName           string    `json:"plan_name"`
	Currency           string    `json:"currency"`
	CurrentPeriodStart time.Time `json:"current_period_start"`
	CurrentPeriodEnd   time.Time `json:"current_period_end"`
}

type CostDashboardMeter struct {
	MeterKey         string              `json:"meter_key"`
	MeterName        string              `json:"meter_name"`
	Unit             string              `json:"unit"`
	Currency         string              `json:"currency"`
	TotalQuantity    string              `json:"total_quantity"`
	TotalAmountCents int64               `json:"total_amount_cents"`
	Rules            []CostDashboardRule `json:"rules"`
}

type CostDashboardRule struct {
	RuleKey           string         `json:"rule_key"`
	DimensionMatch    map[string]any `json:"dimension_match,omitempty"`
	Quantity          string         `json:"quantity"`
	AmountCents       int64          `json:"amount_cents"`
	UnitAmountDecimal *string        `json:"unit_amount_decimal,omitempty"`
}

type CostDashboardTotal struct {
	Currency    string `json:"currency"`
	AmountCents int64  `json:"amount_cents"`
}

// GetByToken resolves the token, composes the usage view, sanitizes,
// and returns the public projection. Returns errs.ErrNotFound when
// the token doesn't match a customer — the handler surfaces this as
// 401 (anti-enumeration).
//
// Empty-state contract: when the customer has no active subscription
// (no_subscription branch), the response carries empty arrays + a
// `billing_period.source = "no_subscription"` instead of a 5xx so
// the embed widget can render a clean empty state.
func (a *CostDashboardAssembler) GetByToken(ctx context.Context, token string) (any, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("empty token")
	}

	cust, err := a.customers.GetByCostDashboardToken(ctx, token)
	if err != nil {
		return nil, err
	}

	// Pin the resolved customer's mode onto ctx before any TxTenant read.
	// The public cost-dashboard route arrives with no mode set, so the
	// downstream RLS livemode predicate would default to live — and every
	// read for a test-mode customer would return nothing (500 in prod).
	// The token lookup runs under TxBypass and is the only place we learn
	// the customer's mode, so we propagate it here.
	ctx = postgres.WithLivemode(ctx, cust.Livemode)

	// Empty-state short-circuit. Without an active sub the usage path
	// requires an explicit window; we don't have one here so we return
	// the empty-state envelope directly.
	subs, _, err := a.subscriptions.List(ctx, subscription.ListFilter{
		TenantID:   cust.TenantID,
		CustomerID: cust.ID,
	})
	if err != nil {
		return nil, fmt.Errorf("list subscriptions: %w", err)
	}
	// A failed credit read fails the response. Showing a zero balance to a
	// customer who holds credit would be wrong and silent.
	credits, err := a.credits.CreditSummary(ctx, cust.TenantID, cust.ID)
	if err != nil {
		return nil, fmt.Errorf("read credits: %w", err)
	}

	activeCount := 0
	for _, s := range subs {
		if s.Status == domain.SubscriptionActive || s.Status == domain.SubscriptionTrialing {
			activeCount++
		}
	}
	if activeCount == 0 {
		credits.EstimatedRemainingCents = estimatedRemaining(credits.BalanceCents, 0)
		return CostDashboardProjection{
			Credits:       credits,
			CustomerID:    cust.ID,
			TenantID:      cust.TenantID,
			BillingPeriod: CostDashboardPeriod{Source: "no_subscription"},
			Subscriptions: []CostDashboardSubscription{},
			Usage:         []CostDashboardMeter{},
			Totals:        []CostDashboardTotal{},
			Livemode:      cust.Livemode,
		}, nil
	}

	res, err := a.usageService.Get(ctx, cust.TenantID, cust.ID, CustomerUsagePeriod{})
	if err != nil {
		return nil, fmt.Errorf("compose usage: %w", err)
	}

	proj := CostDashboardProjection{
		CustomerID: cust.ID,
		TenantID:   cust.TenantID,
		Livemode:   cust.Livemode,
		BillingPeriod: CostDashboardPeriod{
			Start:  res.Period.From,
			End:    res.Period.To,
			Source: "subscription",
		},
		Subscriptions: make([]CostDashboardSubscription, 0, len(res.Subscriptions)),
		Usage:         make([]CostDashboardMeter, 0, len(res.Meters)),
		Totals:        make([]CostDashboardTotal, 0, len(res.Totals)),
	}

	for _, s := range res.Subscriptions {
		proj.Subscriptions = append(proj.Subscriptions, CostDashboardSubscription{
			ID:                 s.ID,
			PlanName:           s.PlanName,
			Currency:           s.Currency,
			CurrentPeriodStart: s.CurrentPeriodStart,
			CurrentPeriodEnd:   s.CurrentPeriodEnd,
		})
	}

	for _, m := range res.Meters {
		rules := make([]CostDashboardRule, 0, len(m.Rules))
		for _, ru := range m.Rules {
			if ru.Unmatched {
				// Unmatched-usage rows are operator information (a
				// pricing-config gap, ADR-044 matrix); the customer-facing
				// projection shows only billed usage.
				continue
			}
			rules = append(rules, CostDashboardRule{
				RuleKey:           ru.RuleKey,
				DimensionMatch:    ru.DimensionMatch,
				Quantity:          ru.Quantity.String(),
				AmountCents:       ru.AmountCents,
				UnitAmountDecimal: ru.UnitAmountDecimal,
			})
		}
		proj.Usage = append(proj.Usage, CostDashboardMeter{
			MeterKey:         m.MeterKey,
			MeterName:        m.MeterName,
			Unit:             m.Unit,
			Currency:         m.Currency,
			TotalQuantity:    m.TotalQuantity.String(),
			TotalAmountCents: m.TotalAmountCents,
			Rules:            rules,
		})
	}

	for _, t := range res.Totals {
		proj.Totals = append(proj.Totals, CostDashboardTotal(t))
		proj.ProjectedTotalCents += t.AmountCents
	}
	credits.EstimatedRemainingCents = estimatedRemaining(credits.BalanceCents, proj.ProjectedTotalCents)
	proj.Credits = credits

	return proj, nil
}
