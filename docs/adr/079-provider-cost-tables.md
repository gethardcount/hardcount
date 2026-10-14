# ADR-079: Provider cost tables + per-customer margin (COGS)

**Status:** Accepted (2026-07-05)
**Context:** `docs/design-cost-tables.md` (locked design — 4-platform
quote-verified sweep + 5-lens adversarial panel; this ADR records the
decisions, the design doc holds the evidence).

## Decision

Model what the OPERATOR pays LLM providers as a first-class, separate
ledger, and report per-customer margin in-app:

1. **`provider_cost_rates`** (migration 0137): per-tenant, per-mode rows
   keyed `(provider, model, token_type)` with `cost_per_token
   NUMERIC(20,12)` — decimal, never float (verified rates reach
   1.5e-06/token). **Current-rate semantics**: one row per key, edited in
   place; NO effective_from (no verified peer effective-dates cost rates;
   trigger to add: first pre-staged price-change ask). Explicit RLS
   ENABLE+FORCE + mode-aware policy + set_livemode trigger.
2. **COGS attaches at INGEST** — the universal verified pattern (snapshot
   semantics). `usage_events.provider_cost_micros BIGINT` (micro-dollars,
   ROUND half-up) + `provider_cost_source` ('table' | 'observed' |
   'not_applicable'), stamped by a scalar subquery inside the single-
   funnel `store.Ingest` INSERT — every path (live, batch, backfill,
   LiteLLM) covered by construction, zero extra round trips. Rate edits
   are non-retroactive; NO recompute/backfill tooling.
3. **Resolution key order:** `model_raw` exact → `model` family → NULL
   (the LiteLLM mapper mixes family tokens and raw ids). NULL-safe
   ordering (`IS NOT DISTINCT FROM`).
4. **Rate-table inference ONLY in phase 1.** LiteLLM's observed cost is
   NOT stamped: it is a whole-call figure fanned across up to 3 per-half
   events (stamping it would multi-count COGS 3×) — and the panel found
   it was never persisted anyway (the "metadata column" was doc-code
   drift, now corrected in payload.go). Named fast-follow rule:
   per-half CostBreakdown only; cache_read and ResponseCost-only
   payloads fall to table inference. *[Amended 2026-10-14] LiteLLM's
   `input_cost` includes cache reads, so the input event taking all of it
   while cache_read was costed from the table counted cache reads twice.
   Now the cache_read event takes `cache_read_cost` and the input event
   takes `input_cost − cache_read_cost`; with cache reads and no
   `cache_read_cost`, both fall to the table. See `observedCostMicros`.*
5. **Honest margin, one surface.** `GET /v1/customers/{id}/margin`:
   headline = total rated USAGE revenue vs total stamped cost (labeled a
   unit-economics view — base fees/credits/taxes excluded); per-model
   revenue + margin ONLY where a pricing rule pins `model` in
   dimension_match; everything else in an explicit
   `unattributed_revenue_cents` bucket — never heuristic allocation.
   Operator-auth only; COGS never renders on the customer-facing cost
   dashboard. `unresolved_events` counts costable-but-unmatched events
   only ('not_applicable' rows are excluded so non-token meters can't
   drown the signal); cache-write exclusion disclosed.
6. **Sourcing: operator CRUD only** (`/v1/provider-costs` + dashboard
   page). No import in phase 1 (trigger: first >10-model operator; file
   upload only, never live-URL fetch). Deleting a rate never rewrites
   stamped history.
7. **Exports:** the operator usage-events CSV carries
   `provider_cost_micros`/`provider_cost_source` (the verified
   warehouse-join fallback).

## Consequences

- Cost is deterministic per event and immune to later rate edits; the
  price of that is honest NULLs for pre-rate history (MANUAL_TEST flow
  orders rates before ingest; UI copy explains).
- Margin % can read near-100% for cheap models — correct: it is usage
  unit economics, not GAAP margin.
- Deferred (triggers named): observed-cost stamping, effective_from,
  import, regex model match + context-size tiers, cost-plus pricing
  (2-2 verified split — not table stakes), per-customer negotiated
  cost rates.
