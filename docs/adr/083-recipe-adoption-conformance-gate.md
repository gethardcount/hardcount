# ADR-083: Recipe adoption conformance gate — refuse to adopt a divergent plan/meter, never silently

**Date:** 2026-07-07
**Status:** Superseded by ADR-085 (2026-07-08) — plans are never adopted by code anymore (each apply mints a fresh born-unique plan), so the conformance gate this ADR built has nothing left to gate

## Summary

A recipe is a ready-made pricing setup applied in one call. This ADR made an apply refuse with 409 when an existing plan or meter with the recipe's code or key had different money settings, instead of silently reusing it. [ADR-085](085-recipe-idempotent-apply.md) replaced it on 2026-07-08: a first apply always creates a new plan and a repeat apply creates nothing, so no plan is ever reused and the plan check is gone. The meter check (the aggregation must match) carried over into ADR-085, which since 2026-07-26 also checks a reused rating rule's money fields. Status: superseded by [ADR-085](085-recipe-idempotent-apply.md).

## Context

`recipe.Service.Instantiate` builds a billing graph (rating rules → meters → pricing rules → plan → dunning → webhook). To stay idempotent and to not clobber an operator's live config, it **adopts** an existing object when one already carries the recipe's key/code: rating rules by `rule_key`, meters by `meter_key`, plans by `code`. Adoption keyed purely on the natural key, with **no check that the adopted object matches what the recipe declares**.

For **plans** this is a money-correctness bug. `plan_code` is operator-chosen (a recipe parameter, default `ai_api_pro`), so a plan with that code can already exist — hand-authored, from another recipe, or the recipe's own plan edited by the operator — with different money-affecting config. `Instantiate` silently adopts it and wires the recipe's meters/pricing rules onto it, returning success. Customers then bill under a timing/amount the operator never declared (observed: an in_advance $99 plan adopted where the recipe declares in_arrears $0). It's reachable through normal flows, breaks the "correct setup in one call" trust the recipe sells, and leaves an internally-inconsistent config. **Meters** have the same shape: `meter.Aggregation` (sum/count/max/last) is billing-consulted (`engine.go` `deferredBucket`/`mapMeterAggregation`), so adopting a same-key meter with a divergent aggregation silently mis-rolls-up usage.

This is the "silent substitution on a money path" our own principle forbids (see memory `feedback_no_silent_fallbacks`, `feedback_adopt_by_key_verify_conformance`).

## Decision

Gate adoption on **conformance**. When `Instantiate` finds an existing plan (by code) or meter (by key), it may adopt only if the existing object's money/behavior-affecting config **matches the recipe's declared spec**. On divergence it **refuses** — returns `errs.AlreadyExists` (HTTP 409) naming the diverging fields, and the transaction rolls back everything created so far. It never mutates the existing object (it may carry live subscriptions) and never wires the recipe's graph onto a plan/meter the operator never declared.

- **Plans** — compare the recipe's *effective* spec (explicit `RecipePlan` fields **plus** implicit defaults) against the existing plan: `currency`, `billing_interval`, `base_amount_cents`, `base_bill_timing`, and the **meter set** (order-insensitive). `RecipePlan` declares no `base_bill_timing`; `CreatePlanTx` defaults empty→`in_arrears`, so the effective declared timing is `domain.BillInArrears` — referenced via the same constant so spec and create-default cannot drift. **Excluded** (cosmetic / operator-owned): `name`, `description`, `status`, `tax_code` — a rename/archive must not block a legitimate reinstall.
- **Meters** — compare `aggregation` only (the sole billing-consulted field); `name`/`unit` are labels.
- **Rating rules** — **unchanged**, adopt-existing with no conformance check. This divergence-tolerance is *deliberate*: a reinstall is not a price change, and republishing/refusing would reprice the operator's live subscriptions at their next period open (customer overrides follow `rule_key`; ADR-070). The gate is scoped to plans + meters precisely because those are what silently change the timing/amount/roll-up a customer bills under; rules are reconnected by ID onto whatever version the operator currently owns.
- The reserved `Force` flag is **not** wired to bypass the gate (that would re-introduce the silent-billing risk); `Force` stays a hard error in v1.

### The four cases

1. **No existing plan** → create (unchanged).
2. **Existing matches the spec** → adopt (idempotent reinstall-after-uninstall happy path). A spec-matching foreign plan also adopts safely — billing is exactly as declared.
3. **Existing diverges, recipe-owned** (operator edited a plan the recipe created, then reinstalls) → **refuse**.
4. **Existing diverges, foreign** (unrelated plan under a colliding code) → **refuse**.

Cases 3 and 4 collapse to one refuse branch today: without a provenance stamp they are indistinguishable, and refuse is the safe answer for both. That collapse is the seam provenance would later split.

## Why refuse, not warn

`feedback_no_silent_fallbacks`: a money path that cannot produce the declared answer fails loudly, never substitutes. Adopting a plan whose config the operator never declared **is** a silent substitution — warn-and-adopt still returns 201 and bills under undeclared timing/amount, and an API/SDK caller never sees the warning. Refuse is the only option that makes the confirmed bug *impossible* rather than merely visible. A strict/dry-run mode and a `Force` escape hatch are deferred (0 customers → refuse-friction is acceptable; adding modes now is over-engineering).

## Deferred: provenance (`source_recipe_key`)

Distinguishing case 3 (recipe-owned edit → could adopt+warn) from case 4 (foreign → refuse) needs a persisted ownership stamp. Deferred — it adds nullable columns across plans/meters/rating_rule_versions plus plumbing, and its only gain *softens* correctness (warn-and-adopt on owned divergence). The conformance gate is the exact substrate provenance would build on, so nothing is wasted. **Named trigger:** the first operator who legitimately edits a recipe-owned plan's money-config and needs reinstall-with-edits to succeed (or the first foreign-collision that must be distinguished from an owned edit). Then add the nullable stamp (NULL = operator-created, no backfill) and flip owned-diverged → adopt-unchanged+warn while foreign-diverged stays refuse.

## Alternatives considered

- **Warn-and-adopt (+ optional strict flag).** Rejected as the default: it still ships wrong billing and hides the divergence from API callers.
- **Provenance now.** Deferred (above) — no named pressure pre-launch, and it softens the correctness posture.
- **Namespaced/derived codes.** Rejected — `plan_code` is operator-chosen and operator-facing; namespacing overrides their choice.
- **Reconcile-to-spec (update the existing plan to match).** Rejected outright — mutating `base_bill_timing`/`base_amount` reprices live subscriptions, the exact clobber this design forbids.

## Consequences

- Silent-wrong-billing via recipe adoption is structurally impossible (plans + meters).
- The one flow that degrades is "operator edits a recipe-owned plan, then reinstalls" → now a 409 that names the diverging fields and the two remedies (reconcile the plan, or instantiate with a different `plan_code`). Provenance (deferred) converts this to adopt+warn later.
- `tax_code` is the one debatable plan exclusion (money-adjacent — affects base-fee tax — but the recipe declares none); promote into the gate on the first design-partner tax-divergence.
- A **reflection drift-guard test** over `domain.Plan` fails if a future field is neither money-checked nor cosmetic-excluded, so a new money field cannot silently escape the gate (`feedback_enforce_invariant_after_bugclass`).
- Preview (`service.go`) does not yet surface the divergence pre-install (it is pure in-memory, no DB read) — a documented fast-follow.

## References
- Memory: `feedback_adopt_by_key_verify_conformance`, `feedback_no_silent_fallbacks`, `feedback_enforce_invariant_after_bugclass`, `feedback_pre_launch_scoping`.
- ADR-070 (rating-rule reconnect by version id — why rule adoption is divergence-tolerant), ADR-031 (base_bill_timing).
