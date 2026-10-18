// Provider cost rates + per-customer margin (ADR-079). COST — what the
// OPERATOR pays their LLM providers — is a separate ledger from PRICE (what
// the customer is billed); nothing here touches invoices or rating. The
// per-event COGS stamp itself lives in store.Ingest (single funnel); this
// file is the operator surface: rate CRUD + the margin report.
package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// AuditEmitter is the narrow in-tx audit seam (ADR-090). Provider-cost rate
// CRUD has no service layer — the handler is the layer that knows intent, so
// it builds the audit.Entry and the store threads the closure onto its own
// transaction: rate write and audit row commit or roll back together.
type AuditEmitter interface {
	LogInTx(ctx context.Context, tx *sql.Tx, e audit.Entry) error
}

// --- store ---

// ListProviderCostRates returns the tenant's current rates (mode-scoped via
// RLS), ordered for the dashboard table.
func (s *PostgresStore) ListProviderCostRates(ctx context.Context, tenantID string) ([]domain.ProviderCostRate, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT id, tenant_id, provider, model, token_type, cost_per_token, currency, created_at, updated_at
		FROM provider_cost_rates
		WHERE tenant_id = $1
		ORDER BY provider, model, token_type
	`, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []domain.ProviderCostRate
	for rows.Next() {
		var r domain.ProviderCostRate
		if err := rows.Scan(&r.ID, &r.TenantID, &r.Provider, &r.Model, &r.TokenType,
			&r.CostPerToken, &r.Currency, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// HasProviderCostRates reports whether the tenant has any provider cost rate
// in the current mode (RLS-scoped). The margin report uses it to tell "no
// rates set up" apart from "rates exist but none matched".
func (s *PostgresStore) HasProviderCostRates(ctx context.Context, tenantID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return false, err
	}
	defer postgres.Rollback(tx)
	var has bool
	err = tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM provider_cost_rates WHERE tenant_id = $1)`, tenantID).Scan(&has)
	return has, err
}

// UpsertProviderCostRate creates or edits-in-place the rate for a key
// (current-rate semantics, ADR-079 D1: the per-event stamp is the history;
// editing a rate only affects FUTURE events).
func (s *PostgresStore) UpsertProviderCostRate(ctx context.Context, tenantID string, r domain.ProviderCostRate) (domain.ProviderCostRate, error) {
	return s.UpsertProviderCostRateAudited(ctx, tenantID, r, nil)
}

// UpsertProviderCostRateAudited is UpsertProviderCostRate with an in-tx audit
// emission hook: the rate write and its audit row commit or roll back together
// (ADR-090 shared fate). The store owns the transaction and exposes it to the
// closure; the caller (the handler — this surface has no service) owns row
// content. emit sees the PERSISTED rate, so the audit row carries the
// store-assigned id and the values as they actually landed. nil emit =
// unaudited upsert (unit-test / non-request callers).
//
// The emission is unconditional on success by construction: an
// INSERT … ON CONFLICT DO UPDATE … RETURNING that scans a row always wrote
// one (a same-values re-PUT still bumps updated_at — a real mutation), so
// there is no zero-row arm to fabricate evidence for.
func (s *PostgresStore) UpsertProviderCostRateAudited(
	ctx context.Context, tenantID string, r domain.ProviderCostRate,
	emit func(tx *sql.Tx, out domain.ProviderCostRate) error,
) (domain.ProviderCostRate, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.ProviderCostRate{}, err
	}
	defer postgres.Rollback(tx)

	err = tx.QueryRowContext(ctx, `
		INSERT INTO provider_cost_rates (tenant_id, provider, model, token_type, cost_per_token, currency)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, livemode, provider, model, token_type)
		DO UPDATE SET cost_per_token = EXCLUDED.cost_per_token,
		              currency = EXCLUDED.currency,
		              updated_at = now()
		RETURNING id, tenant_id, provider, model, token_type, cost_per_token, currency, created_at, updated_at
	`, tenantID, r.Provider, r.Model, r.TokenType, r.CostPerToken, r.Currency,
	).Scan(&r.ID, &r.TenantID, &r.Provider, &r.Model, &r.TokenType,
		&r.CostPerToken, &r.Currency, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return domain.ProviderCostRate{}, err
	}
	if emit != nil {
		if err := emit(tx, r); err != nil {
			return domain.ProviderCostRate{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.ProviderCostRate{}, err
	}
	return r, nil
}

// DeleteProviderCostRate removes a rate row. Stamped events keep their
// snapshot (documented — deletion never rewrites history).
func (s *PostgresStore) DeleteProviderCostRate(ctx context.Context, tenantID, id string) error {
	return s.DeleteProviderCostRateAudited(ctx, tenantID, id, nil)
}

// DeleteProviderCostRateAudited is DeleteProviderCostRate with an in-tx audit
// emission hook (ADR-090 shared fate). Two properties matter here:
//
//   - emit runs ONLY on a row that actually vanished. DELETE … RETURNING
//     yields a row iff exactly one was removed; a miss is sql.ErrNoRows →
//     errs.ErrNotFound with no emission, so deleting a nonexistent rate can
//     never fabricate a "deleted" record.
//   - emit receives the DELETED row, read inside the tx. The row is gone
//     afterwards, so the audit entry is the only surviving description of what
//     the operator removed — an id alone would be unresolvable forever.
func (s *PostgresStore) DeleteProviderCostRateAudited(
	ctx context.Context, tenantID, id string,
	emit func(tx *sql.Tx, deleted domain.ProviderCostRate) error,
) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	var deleted domain.ProviderCostRate
	err = tx.QueryRowContext(ctx, `
		DELETE FROM provider_cost_rates
		WHERE tenant_id = $1 AND id = $2
		RETURNING id, tenant_id, provider, model, token_type, cost_per_token, currency, created_at, updated_at
	`, tenantID, id).Scan(&deleted.ID, &deleted.TenantID, &deleted.Provider, &deleted.Model,
		&deleted.TokenType, &deleted.CostPerToken, &deleted.Currency, &deleted.CreatedAt, &deleted.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return errs.ErrNotFound
	}
	if err != nil {
		return err
	}
	if emit != nil {
		if err := emit(tx, deleted); err != nil {
			return fmt.Errorf("audit emission: %w", err)
		}
	}
	return tx.Commit()
}

// CustomerCostByModel aggregates the stamped COGS for one customer's window:
// per-model cost plus the honesty counters (ADR-079 D7 — 'unresolved' =
// token events that carried costable dims but matched no rate: the
// actionable signal; 'not_applicable' events are excluded so they can't
// drown it).
type CustomerCostByModel struct {
	Model      string
	CostMicros int64
	Events     int64
}

func (s *PostgresStore) CustomerProviderCost(ctx context.Context, tenantID, customerID string, from, to time.Time) (byModel []CustomerCostByModel, unresolvedEvents int64, err error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, 0, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT COALESCE(properties->>'model', properties->>'model_raw', '(unknown)') AS model,
			COALESCE(SUM(provider_cost_micros), 0)::BIGINT,
			COUNT(*)
		FROM usage_events
		WHERE tenant_id = $1 AND customer_id = $2
		  AND timestamp >= $3 AND timestamp < $4
		  AND provider_cost_micros IS NOT NULL
		GROUP BY 1
		ORDER BY 2 DESC
	`, tenantID, customerID, from, to)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var m CustomerCostByModel
		if err := rows.Scan(&m.Model, &m.CostMicros, &m.Events); err != nil {
			return nil, 0, err
		}
		byModel = append(byModel, m)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	// Unresolved = costable-but-unmatched: source is NULL AND no stamp.
	// ('not_applicable' rows are excluded by the source IS NULL predicate.)
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM usage_events
		WHERE tenant_id = $1 AND customer_id = $2
		  AND timestamp >= $3 AND timestamp < $4
		  AND provider_cost_micros IS NULL AND provider_cost_source IS NULL
	`, tenantID, customerID, from, to).Scan(&unresolvedEvents)
	if err != nil {
		return nil, 0, err
	}
	return byModel, unresolvedEvents, nil
}

// --- margin assembly ---

// MarginModelRow is one per-model line of the margin report. RevenueCents
// and MarginBps are set ONLY when the model's revenue is honestly
// attributable (a pricing rule pins `model` in dimension_match); otherwise
// Attributed=false and only cost renders — never a heuristic allocation
// (ADR-079 D6).
type MarginModelRow struct {
	Model        string `json:"model"`
	CostMicros   int64  `json:"cost_micros"`
	RevenueCents int64  `json:"revenue_cents,omitempty"`
	Attributed   bool   `json:"attributed"`
	// MarginBps is this model's margin, by the same formula as the
	// report's headline (marginBps). Omitted whenever the headline is
	// (currency, unresolved cost) and also when the row has no attributed
	// revenue or no stamped cost of its own: either way it is a margin
	// nobody measured.
	MarginBps *int64 `json:"margin_bps,omitempty"`
}

// marginBps is (revenue − cost) / revenue in basis points, rounded half
// away from zero. Cost is micro-dollars, revenue cents. Nil when there is
// no revenue to divide by. The headline and every per-model row use it, so
// a row and the total can never disagree on the arithmetic.
func marginBps(revenueCents, costMicros int64) *int64 {
	if revenueCents <= 0 {
		return nil
	}
	costCents := decimal.NewFromInt(costMicros).Div(decimal.NewFromInt(10000)) // micros → cents (1e4)
	revCents := decimal.NewFromInt(revenueCents)
	bps := revCents.Sub(costCents).Div(revCents).Mul(decimal.NewFromInt(10000)).Round(0).IntPart()
	return &bps
}

type MarginReport struct {
	CustomerID string    `json:"customer_id"`
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`
	// Headline (always correct): total rated usage revenue vs total
	// stamped provider cost for the window. Rated USAGE revenue — base
	// fees, credits, and taxes are not in this number (it is a usage
	// unit-economics view, not GAAP margin; the UI copy says so).
	RevenueCents int64 `json:"revenue_cents"`
	// RevenueCurrency is the currency of RevenueCents: the currency of the
	// price rules that rated it (ADR-100 keeps a customer's prices in one
	// currency). Empty when the window's revenue spans more than one
	// currency, or when the customer has no priced meter.
	RevenueCurrency string `json:"revenue_currency,omitempty"`
	CostMicros      int64  `json:"cost_micros"`
	// CostCurrency is the currency of CostMicros. Always USD: the stamp
	// treats every rate as dollars (provider_cost_micros is micro-dollars)
	// and LiteLLM reports its cost in dollars.
	CostCurrency string `json:"cost_currency"`
	// MarginBps = (revenue − cost) / revenue in basis points; omitted
	// when revenue is 0, when revenue is not in CostCurrency (a margin
	// across two currencies would be a made-up number), when no event
	// in the window carries a provider cost (CostedEvents == 0), and when
	// some event's cost is unknown (UnresolvedEvents > 0). Without a cost
	// the formula reads 100%; with part of the cost missing it reads too
	// high. Either way it is a margin nobody measured.
	MarginBps *int64 `json:"margin_bps,omitempty"`
	// CostedEvents counts the window's events that carry a stamped provider
	// cost (zero cost included). Zero means the cost side is unknown, not
	// free.
	CostedEvents int64 `json:"costed_events"`
	// CostRatesConfigured is whether the tenant has any provider cost rate
	// at all. False with CostedEvents == 0 means "set up Provider costs";
	// true means the rates exist but none matched this usage.
	CostRatesConfigured      bool             `json:"cost_rates_configured"`
	ByModel                  []MarginModelRow `json:"by_model"`
	UnattributedRevenueCents int64            `json:"unattributed_revenue_cents"`
	UnresolvedEvents         int64            `json:"unresolved_events"`
	CacheWriteExcluded       bool             `json:"cache_write_excluded"`
}

// MarginAssembler joins stamped COGS with rated usage revenue.
type MarginAssembler struct {
	store    *PostgresStore
	usageSvc *CustomerUsageService
}

func NewMarginAssembler(store *PostgresStore, usageSvc *CustomerUsageService) *MarginAssembler {
	return &MarginAssembler{store: store, usageSvc: usageSvc}
}

// effectiveNow is the customer's current time for the default margin
// window: the usage Service's effectiveNow, the same resolution the
// usage summary's default month uses (test-clock frozen_time when the
// customer is pinned, wall-clock otherwise).
func (a *MarginAssembler) effectiveNow(ctx context.Context, tenantID, customerID string) time.Time {
	if a != nil && a.usageSvc != nil && a.usageSvc.usage != nil {
		return a.usageSvc.usage.effectiveNow(ctx, tenantID, customerID)
	}
	return time.Now().UTC()
}

func (a *MarginAssembler) Get(ctx context.Context, tenantID, customerID string, from, to time.Time) (MarginReport, error) {
	rep := MarginReport{CustomerID: customerID, From: from, To: to, CostCurrency: "USD", CacheWriteExcluded: true, ByModel: []MarginModelRow{}}

	byModel, unresolved, err := a.store.CustomerProviderCost(ctx, tenantID, customerID, from, to)
	if err != nil {
		return MarginReport{}, fmt.Errorf("aggregate provider cost: %w", err)
	}
	rep.UnresolvedEvents = unresolved
	rep.CostRatesConfigured, err = a.store.HasProviderCostRates(ctx, tenantID)
	if err != nil {
		return MarginReport{}, fmt.Errorf("provider cost rates: %w", err)
	}

	// Revenue: rated usage for the same window, attributed per model ONLY
	// where the rule's dimension_match pins model.
	usage, err := a.usageSvc.Get(ctx, tenantID, customerID, CustomerUsagePeriod{From: from, To: to})
	if err != nil {
		return MarginReport{}, fmt.Errorf("rate usage window: %w", err)
	}
	revenueByModel := map[string]int64{}
	// Currencies of the meters that earned revenue; with no revenue, of
	// every priced meter, so an empty window still names the currency.
	earned, priced := map[string]bool{}, map[string]bool{}
	for _, m := range usage.Meters {
		if m.Currency != "" {
			priced[m.Currency] = true
			if m.TotalAmountCents != 0 {
				earned[m.Currency] = true
			}
		}
		for _, r := range m.Rules {
			model, ok := r.DimensionMatch["model"].(string)
			if ok && model != "" {
				revenueByModel[model] += r.AmountCents
			} else {
				rep.UnattributedRevenueCents += r.AmountCents
			}
			rep.RevenueCents += r.AmountCents
		}
	}

	currencies := earned
	if len(currencies) == 0 {
		currencies = priced
	}
	if len(currencies) == 1 {
		for c := range currencies {
			rep.RevenueCurrency = c
		}
	}

	// A margin needs revenue and cost in one currency and every event's
	// cost known. A row also needs its own revenue and its own stamped cost
	// (every byModel entry has one: the query keeps only costed events).
	measurable := rep.RevenueCurrency == rep.CostCurrency && rep.UnresolvedEvents == 0

	seen := map[string]bool{}
	for _, c := range byModel {
		rep.CostMicros += c.CostMicros
		rep.CostedEvents += c.Events
		row := MarginModelRow{Model: c.Model, CostMicros: c.CostMicros}
		if rev, ok := revenueByModel[c.Model]; ok {
			row.RevenueCents, row.Attributed = rev, true
			if measurable {
				row.MarginBps = marginBps(rev, c.CostMicros)
			}
		}
		rep.ByModel = append(rep.ByModel, row)
		seen[c.Model] = true
	}
	// Models with attributed revenue but no stamped cost still render.
	for model, rev := range revenueByModel {
		if !seen[model] {
			rep.ByModel = append(rep.ByModel, MarginModelRow{Model: model, RevenueCents: rev, Attributed: true})
		}
	}

	// Largest bill first; the map walk above has no order of its own.
	sort.SliceStable(rep.ByModel, func(i, j int) bool {
		a, b := rep.ByModel[i], rep.ByModel[j]
		if a.RevenueCents != b.RevenueCents {
			return a.RevenueCents > b.RevenueCents
		}
		if a.CostMicros != b.CostMicros {
			return a.CostMicros > b.CostMicros
		}
		return a.Model < b.Model
	})

	if measurable && rep.CostedEvents > 0 {
		rep.MarginBps = marginBps(rep.RevenueCents, rep.CostMicros)
	}
	return rep, nil
}

// --- handler ---

// ProviderCostHandler is the operator surface: rate CRUD + margin report.
// Operator-auth only — COGS never renders on customer-facing pages.
type ProviderCostHandler struct {
	store       *PostgresStore
	margin      *MarginAssembler
	auditLogger AuditEmitter
}

func NewProviderCostHandler(store *PostgresStore, margin *MarginAssembler) *ProviderCostHandler {
	return &ProviderCostHandler{store: store, margin: margin}
}

// SetAuditLogger wires in-tx audit emission for the rate mutations (ADR-090).
// There is no provider-cost service, so the handler builds the entry — it is
// the layer that knows the operator's intent — and hands it to the store's
// …Audited variants, which run it on the write's own transaction. A nil
// emitter skips emission (keeps handler unit tests fake-friendly); the
// composition root's audit.MustWired check is what makes a forgotten wiring
// line fail loudly at boot instead of silently un-auditing the routes.
func (h *ProviderCostHandler) SetAuditLogger(a AuditEmitter) { h.auditLogger = a }

func (h *ProviderCostHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.list)
	r.Put("/", h.upsert)
	r.Delete("/{id}", h.delete)
	return r
}

func (h *ProviderCostHandler) list(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	rates, err := h.store.ListProviderCostRates(r.Context(), tenantID)
	if err != nil {
		respond.FromError(w, r, err, "provider_cost_rate")
		return
	}
	if rates == nil {
		rates = []domain.ProviderCostRate{}
	}
	respond.JSON(w, r, http.StatusOK, map[string]any{"data": rates})
}

func (h *ProviderCostHandler) upsert(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	var in domain.ProviderCostRate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		respond.BadRequest(w, r, "invalid JSON")
		return
	}
	in.Provider = strings.TrimSpace(strings.ToLower(in.Provider))
	in.Model = strings.TrimSpace(in.Model)
	in.TokenType = strings.TrimSpace(strings.ToLower(in.TokenType))
	if in.Provider == "" || in.Model == "" || in.TokenType == "" {
		respond.FromError(w, r, errs.Required("provider, model, token_type"), "provider_cost_rate")
		return
	}
	if in.CostPerToken.IsNegative() {
		respond.FromError(w, r, errs.Invalid("cost_per_token", "must be zero or positive"), "provider_cost_rate")
		return
	}
	if in.Currency == "" {
		in.Currency = "USD"
	}
	in.Currency = strings.ToUpper(in.Currency)
	out, err := h.store.UpsertProviderCostRateAudited(r.Context(), tenantID, in, h.upsertEmission(r.Context()))
	if err != nil {
		respond.FromError(w, r, err, "provider_cost_rate")
		return
	}
	respond.JSON(w, r, http.StatusOK, out)
}

// upsertEmission builds the in-tx audit row for PUT /v1/provider-costs. Wire
// strings are FROZEN vocabulary: action "update" (the route is an upsert —
// the operator is setting the current rate for a key; ADR-079 D1 gives the
// row edit-in-place semantics) and resource_type "provider_cost". Metadata
// carries the full rate so a post-mortem can answer "what did this cost
// become, and what was it billed against?" without the row still existing.
func (h *ProviderCostHandler) upsertEmission(ctx context.Context) func(tx *sql.Tx, out domain.ProviderCostRate) error {
	if h.auditLogger == nil {
		return nil
	}
	return func(tx *sql.Tx, out domain.ProviderCostRate) error {
		return h.auditLogger.LogInTx(ctx, tx, audit.Entry{
			Action:        domain.AuditActionUpdate,
			ResourceType:  "provider_cost",
			ResourceID:    out.ID,
			ResourceLabel: providerCostLabel(out),
			Metadata: map[string]any{
				"provider":       out.Provider,
				"model":          out.Model,
				"token_type":     out.TokenType,
				"cost_per_token": out.CostPerToken.String(),
				"currency":       out.Currency,
			},
		})
	}
}

func (h *ProviderCostHandler) delete(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	if err := h.store.DeleteProviderCostRateAudited(r.Context(), tenantID, chi.URLParam(r, "id"), h.deleteEmission(r.Context())); err != nil {
		respond.FromError(w, r, err, "provider_cost_rate")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteEmission builds the in-tx audit row for DELETE
// /v1/provider-costs/{id}. It only ever runs when a row was actually removed
// (the store's DELETE … RETURNING gate), and it describes the row that was
// removed — the deleted rate is unrecoverable, so the metadata IS the record.
func (h *ProviderCostHandler) deleteEmission(ctx context.Context) func(tx *sql.Tx, deleted domain.ProviderCostRate) error {
	if h.auditLogger == nil {
		return nil
	}
	return func(tx *sql.Tx, deleted domain.ProviderCostRate) error {
		return h.auditLogger.LogInTx(ctx, tx, audit.Entry{
			Action:        domain.AuditActionDelete,
			ResourceType:  "provider_cost",
			ResourceID:    deleted.ID,
			ResourceLabel: providerCostLabel(deleted),
			Metadata: map[string]any{
				"provider":       deleted.Provider,
				"model":          deleted.Model,
				"token_type":     deleted.TokenType,
				"cost_per_token": deleted.CostPerToken.String(),
				"currency":       deleted.Currency,
			},
		})
	}
}

// providerCostLabel is the human-readable identity of a rate in the audit
// list ("anthropic / claude-sonnet-4 (input)") — the rate's natural key, since
// its id means nothing to an operator reading the log.
func providerCostLabel(r domain.ProviderCostRate) string {
	return fmt.Sprintf("%s / %s (%s)", r.Provider, r.Model, r.TokenType)
}

// Margin serves GET /v1/customers/{id}/margin?from&to (operator auth;
// mounted from the router next to the other customer subresources).
func (h *ProviderCostHandler) Margin(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	customerID := chi.URLParam(r, "id")
	from, to, err := h.marginWindow(r, tenantID, customerID)
	if err != nil {
		respond.BadRequest(w, r, err.Error())
		return
	}
	rep, err := h.margin.Get(r.Context(), tenantID, customerID, from, to)
	if err != nil {
		respond.FromError(w, r, err, "margin")
		return
	}
	respond.JSON(w, r, http.StatusOK, rep)
}

// marginWindow is the Margin handler's [from, to): the request's bounds,
// with missing ones defaulted around the customer's effective now.
func (h *ProviderCostHandler) marginWindow(r *http.Request, tenantID, customerID string) (time.Time, time.Time, error) {
	return parseWindow(r, func() time.Time {
		return h.margin.effectiveNow(r.Context(), tenantID, customerID)
	})
}

// parseWindow reads from/to (RFC3339); a missing bound defaults to the
// last 30 days ending at now(). now is the CUSTOMER's effective now —
// frozen_time for a customer pinned to a test clock — so a bare request
// for a clock-pinned customer covers the simulated last 30 days, the
// same timeline the usage summary and customer usage follow. It is
// called only when a bound is missing, so an explicit window costs no
// clock lookup.
func parseWindow(r *http.Request, now func() time.Time) (time.Time, time.Time, error) {
	q := r.URL.Query()
	var from, to time.Time
	if q.Get("from") == "" || q.Get("to") == "" {
		n := now()
		from, to = n.AddDate(0, 0, -30), n
	}
	if v := r.URL.Query().Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("from: must be RFC3339")
		}
		from = t
	}
	if v := r.URL.Query().Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("to: must be RFC3339")
		}
		to = t
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, fmt.Errorf("from must be before to")
	}
	return from, to, nil
}
