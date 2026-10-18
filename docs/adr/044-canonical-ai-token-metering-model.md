# ADR-044: Canonical AI token-metering model (one `tokens` meter + `token_type` dimension)

**Status:** Accepted
**Date:** 2026-06-01
**Related:** ADR-033 (LiteLLM spend adapter), multi-dimensional metering (`internal/usage` `AggregateByPricingRules`), pricing recipes (`internal/recipe`), `project_positioning_wedge.md`

## Summary

All AI token usage is metered on one `tokens` meter (aggregation `sum`), with the token role in a `token_type` dimension: `input`, `output`, `cache_read`, `cache_write_5m`, `cache_write_1h`, and more as needed. `model` and `provider` are dimensions too, and prices are pricing rules on that meter that match `{model, token_type}`, so a new model or role adds rules, never meters. Each ingestion mapper must turn a provider's counts into separate, non-overlapping roles (input is prompt tokens minus cached tokens), so the rating engine only sums. Today the LiteLLM mapper emits `input`, `output` and `cache_read`, and no cache-write role, because LiteLLM does not report the 5-minute and 1-hour split. Cache-write tokens bill as `input` only when LiteLLM counts them in prompt tokens, as it does for Anthropic and Bedrock. Status: active.

## Context

Velox has two incompatible shapes for LLM token usage today, and they don't connect:

- **Pricing recipes** (`anthropic_style`, `openai_style`) create **one** `tokens` meter and price via dimensions `operation=input|output` + boolean `cached=true|false` + `model`, with priority-matched rating rules (the `cached` variant wins via `priority=200`).
- **The LiteLLM ingestion mapper** (`internal/integrations/litellm/mapper.go`) emits **two** meters — `tokens_input` and `tokens_output` — with dimensions `model` + `provider`, no `operation`/`cached`.

So LiteLLM-ingested events never rate against the recipe rules: the headline wedge demo ("point your LiteLLM proxy at Velox and your AI-native plan bills automatically") cannot work end-to-end. Picking the canonical model is a re-litigable architecture decision, hence this ADR. We must define **one** metering model that every ingestion path (LiteLLM today; OpenAI/Bedrock/Vertex/direct tomorrow) and every consumer (recipes, rating rules, cost dashboard) conforms to — so the next provider doesn't re-open this mismatch.

### Verified industry research (2026-06-01)

Six sourced agents (metering platforms + the providers' own billing itemization):

- **Metering platforms converge on ONE token meter, role as a DIMENSION, priced by matrix/dimensional ("GROUP BY then price") pricing:**
  - **Lago** (high): *"Create a single billable metric to track token usage across different models and input/output types"* — `type:[input,output]` is a filter value, model another filter; "one metric, many prices, no duplicated metrics."
  - **OpenMeter** (high): one `tokens_total` meter, `groupBy:{provider,model,type}`; *"We recommend using groups instead of creating multiple meters."*
  - **Orb / Metronome** (medium): one event per call carrying each role as a property; model/provider as dimensional-pricing group keys. They lean toward one rate per role at the pricing layer — which in Velox is the *same thing* as one meter with N rules matching on the role dimension.
- **Provider APIs (OpenAI, Anthropic, high) itemize roles as separate named FIELDS** (`prompt_tokens`/`completion_tokens`; `uncached_input_tokens`/`output_tokens`/`cache_read_input_tokens`/`cache_creation`). This is a per-call *reporting* itemization, **not** a metering data model — even there `model` is a `group_by` dimension (never baked into the field name) and `provider` doesn't exist (each API is first-party). A metering platform consumes this itemization at the mapper boundary.

Velox's own storage already **is** the metering-platform model: one meter, scalar summed quantity, dimensions in `usage_events.properties` (JSONB), rating via `e.properties @> dimension_match`. So this ADR ratifies the model Velox is structurally built for and deletes the two-meter anomaly.

### The binding constraint: Anthropic's cache roles

The provider itemization sets the hard floor on what the model must express. Anthropic meters and **prices** five disjoint roles per model:

| Role | Anthropic rate (relative to base input) |
|---|---|
| `input` (uncached) | 1× |
| `output` | model-specific (≈3–5×) |
| `cache_read` | 0.1× |
| `cache_write_5m` | 1.25× |
| `cache_write_1h` | 2× |

A single boolean `cached:true|false` (what **both** recipes use today) **provably cannot** distinguish cache-read from 5-minute-write from 1-hour-write — three different prices. So the role dimension must be an open string enum, not a boolean.

OpenAI adds a normalization wrinkle: its `cached_tokens` is a **subset** of `prompt_tokens` (already counted), not additive — a mapper that naively emits both double-counts the cached portion.

## Decision

**Canonical metering model for all AI token usage:**

1. **One meter, key `tokens`** (unit: `tokens`, aggregation: `sum`). No per-role meters.
2. **Token role is a single dimension `token_type`** on `usage_events.properties` — an **open string enum**, seeded `{input, output, cache_read, cache_write_5m, cache_write_1h}` and extensible (`reasoning`, `audio`, `system`, …).
3. **`model` and `provider` are dimensions** (`provider` is a Velox-only attribution dimension; no provider API prices by provider).
4. **Pricing is matrix/dimensional**, unchanged mechanism: N `meter_pricing_rules` on the `tokens` meter, each `dimension_match` a JSONB subset over `{model, token_type}` (e.g. `{model:'claude-opus-4', token_type:'cache_write_1h'}`), resolved by the existing priority+claim `e.properties @> dimension_match`. Because `token_type` values are **disjoint**, each `(model, role)` is exactly one rule at one priority — the recipes no longer need the `priority=200` cached-wins trick.
5. **Mapper-normalization invariant:** every provider mapper is solely responsible for normalizing provider-specific itemization into **additive-disjoint** canonical roles — `uncached_input = prompt_tokens − cached_tokens`, Anthropic `cache_creation` TTL split into `cache_write_5m`/`cache_write_1h`. The **rating engine never does role arithmetic**; `SUM(quantity)` partitioned by `token_type` is always correct. This keeps OpenAI-style subset semantics out of the engine and out of every future mapper's contract.

No schema migration: `usage_events.properties` and `meter_pricing_rules.dimension_match` are free-form JSONB. Pre-launch, local-only, zero live integrations — clean change, **no backfill** (per `feedback_no_speculative_backfill`).

### Conformance

- **Ingestion** (`internal/integrations/litellm/mapper.go` + every future mapper): emit meter key `tokens`, one event **per present role**, `token_type` on `properties`. Replace `MeterKeyTokensInput`/`MeterKeyTokensOutput` with `MeterKeyTokens = "tokens"`; `prompt_tokens → {token_type:input}`, `completion_tokens → {token_type:output}`; idempotency key `p.ID + ":" + token_type`. `model`/`provider` dims already correct. Cache roles require extending `payload.go`'s `Usage` struct to parse `prompt_tokens_details.cached_tokens` (and Anthropic `cache_creation` TTLs when LiteLLM forwards them) — see Open Decisions for v1 scope.
- **Recipes** (`anthropic_style.yaml`, `openai_style.yaml`): rename the role dimension `operation → token_type`; replace `cached:true` rules with `token_type` role values (`cache_read`, and for Anthropic `cache_write_5m`/`cache_write_1h`). Meter/rule *shape* is unchanged (one meter, many rules, `dimension_match` subset). Drop the `priority=200` cached-wins comment/trick.
- **Cost dashboard** (`internal/usage/cost_dashboard.go`): no structural change — it already lists one row per meter key with a per-rule `DimensionMatch` breakdown. Under this model it shows one `tokens` meter grouped by `{model, token_type}`, matching how OpenAI/Anthropic's own usage APIs group. Only cosmetic work: finance-readable labels for `token_type` values (per `feedback_ui_copy_no_engineering_jargon`).
- **ADR-033** (LiteLLM adapter): the `prompt_tokens→tokens_input / completion_tokens→tokens_output` mapping table (lines ~28–32) is superseded by this ADR — correct it in the same change (doc discipline).

## Decisions locked (2026-06-01)

1. **Cache-write TTL granularity** → **carry `cache_write_5m` + `cache_write_1h` separately** (full Anthropic parity: 1.25× vs 2×).
2. **v1 scope / cadence** → **adopt the canonical dimension model now**, ship **input/output end-to-end** immediately, define all cache roles in the enum + recipes now; **LiteLLM cache-token parsing is a fast-follow** (the payload doesn't parse `prompt_tokens_details.cached_tokens` / Anthropic `cache_creation` yet). Until that lands, LiteLLM emits only `input`/`output`; direct-ingest operators get full cache parity via the recipe rules immediately.
3. **Dimension name** → **`token_type`** (matches OpenMeter's `type`).
4. **`provider`** → **attribution/reporting dimension only**; pricing rules key on `{model, token_type}`. (Revisit if an operator needs to price the same model differently across providers.)
5. **`token_type` validation locus** → **engine storage stays free-form; validate `token_type` against the known enum at the mapper boundary.**

v1 `token_type` enum: `{input, output, cache_read, cache_write_5m, cache_write_1h}`.

## Consequences

**Positive:** one metering model across all providers; expresses 5+ roles × N models × cache without a meter explosion (adding a model/role adds *rules*, never meters); unblocks the wedge demo; deletes the two-meter anomaly and the `priority` cached-hack; cost dashboard rolls up cleanly under one `tokens` headline.

**Negative:** full Anthropic cache parity requires the mapper to parse and normalize provider-specific cache itemization (real work, isolated to the mapper). The `token_type` enum is convention-enforced (validated at the mapper), not schema-enforced — consistent with the multi-dim design's free-form-dimension stance.

**Neutral:** the recipe and mapper both change, but the *storage* model and rating mechanism are untouched — this ratifies what Velox already is.
