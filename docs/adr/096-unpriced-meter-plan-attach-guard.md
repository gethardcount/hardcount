# ADR-096: Unpriced meter on a plan — guard at authoring, loud at finalize

Date: 2026-07-18
Status: Accepted (amended 2026-10-14; see [the amendment](#amendment-2026-10-14-981-refuse-removing-the-last-price-and-count-unpriced-usage)). Current rule: an invoice is still built without usage that has no price. Removing a meter's last price is refused while a live subscription's plan uses the meter, and every invoice built without priceless usage logs it and counts its quantity.

Original status: Accepted. The **guard + loud-log** below is IMPLEMENTED in this PR. The fuller "durable marker + critical banner + metering-only flag" design (from the design panel) is **deferred** — see *Deferred* — because live-doc verification showed it has no industry precedent and the guard already fixes the real defect.
Relates: ADR-070 (no-silent-fallbacks in billing — this extends its fail-LOUD mandate to the finalize path), ADR-044 (multi-dim `{model,token_type}` rating — the meters this most affects), ADR-034 (plan immutability — the guard runs after it).

## Context

A MANUAL_TEST FLOW S1 walkthrough surfaced a **silent under-bill**: when a plan attaches a usage meter that has **no rating rule** — neither a `meter_pricing_rules` binding nor a default `meters.rating_rule_version_id` — the billing engine drops that meter's usage at cycle-close finalize and charges the customer **$0** for it, with no signal. Reproduced live: a $29 plan + 1,000 metered events that should have added $10.00 finalized and **auto-charged** base+tax only. The events aggregated correctly (1,000 units); they were simply never rated.

The trap is a **key-match illusion**: the meter and the rule both carried the key `api_calls`, so it *looked* wired — but Velox binds meter↔rule by an explicit row/field, not by matching keys. The meter was created before the rule, so it was never bound.

### Loudness was inverted

- **Preview** (`preview.go`) warns, citing ADR-070.
- **Usage read endpoint** (`customer_usage.go:411`) warns "no rating rule binding — skipped from totals".
- **Finalize + auto-charge** — the money-moving path — was the *quietest*: `engine.go:2917` (cycle) and `:4177` (cancel) were bare silent `continue`s. Their multi-dim siblings (`:2837`, `:4100`) at least `slog.Warn`. No plan/subscription guard caught it upstream either.

### Design panel, then industry verification

A four-stance adversarial panel (annotate-only, fail-closed/hold, config-guard, durable-record) synthesized a fuller design: a durable `invoices.unrated_usage` marker + a **critical** `usage_unrated` attention banner + a `metering_only` meter flag, plus the guard. Before building it, the "is this industry-standard?" question was checked against live docs — and the answer **reshaped the decision**:

- **The meter/rule split is universal** (Stripe meter+price, Metronome product+rate-card, Lago metric+charge, Orb metric+price) — Velox's architecture is standard.
- **"Unpriced meter = tracked but not billed" is a *documented, intended* state in Orb and Lago.** Orb: *"Without a price configured for a metric on a given plan, the usage is **tracked but not billed**"* ([docs.withorb.com](https://docs.withorb.com/self-serve/agent-pricing)). Lago: a metric with no *charge* isn't invoiced ([getlago.com](https://www.getlago.com/docs/guide/plans/charges)). So Velox's *current* silent behavior actually matches Orb/Lago; only Stripe binds tightly enough (you subscribe to a **price**, not a bare meter) to make the state hard to reach.
- **No platform documents a "warn about unbilled usage" feature.** The marker/banner has **no industry precedent**.

Conclusion: the fuller marker/banner apparatus would make Velox *louder than anyone* about a state Orb/Lago treat as normal. The real, Velox-specific defect is narrower — the **key-match illusion** that lets an operator attach a bare unpriced meter to a plan and *believe* it's priced. The fix is to make pricing an **explicit part of attaching a meter** (as Stripe/Orb/Lago do), not to detect-and-annotate after charging.

## Decision

Two things, both minimal:

### 1. Guard at plan-attach (the fix)

`pricing.Service.CreatePlan` and `UpdatePlan` reject attaching a meter that isn't priced — a new `assertMetersPriced` helper requires each attached meter to carry either a default rating-rule binding or ≥1 `meter_pricing_rules` row. Error: `meter %q has no rating rule — bind one … before attaching it to a plan, or its usage will bill $0`.

- **Entirely within the pricing domain** (plans + meters both live there) — no subscription→pricing edge, no `arch/boundaries` allowlist change. Subscriptions inherit meters transitively via the plan, so guarding plan-attach covers sub-create without touching the subscription mutation sites.
- **Recipes pass unchanged** — the recipe apply path creates the rule + meter + binding *before* the plan (verified: `recipe/service.go` order is rule→meter→binding→plan), so it never produces an unpriced meter.
- In `UpdatePlan` it runs **after** the ADR-034 immutability guard, so a live-plan meter change still returns the immutability error first.
- As a bonus it also rejects a **non-existent** meter id (previously unvalidated) as a clean `meter_ids` validation error.

This mirrors the industry norm (Stripe *rate*, Orb/Lago *price*/*charge* as explicit plan components) and removes the key-match illusion: you can no longer attach a bare unpriced meter to a plan.

### 2. Make the finalize skip loud, not silent

The two bare `continue`s (`engine.go:2917` cycle, `:4177` cancel) now `slog.Warn` — matching their multi-dim siblings. The guard prevents the common case at authoring, so reaching this at finalize means genuine **drift** (a rule unbound out from under a live sub). It's cheap insurance that satisfies ADR-070's fail-LOUD mandate for the one case the guard can't see — with no invoice-schema change.

## Deferred (considered, not built)

The panel's fuller design — a durable `invoices.unrated_usage` JSONB marker, a **critical** `usage_unrated` attention banner + classifier restructure, the empty-invoice ($0-base) fix, and a `meters.metering_only` flag + invariant — is **deferred**, because:

- **No industry precedent** for surfacing unbilled usage (verified above); and Orb/Lago treat unpriced-metered as an *intended* state, so a critical banner would over-signal.
- **The guard already prevents the reachable case.** The marker only adds value for post-creation drift, which has no named pressure at 0 customers (and is now non-silent via the loud log).

**Triggers to revisit:**
- **Intentional metering-only** (free-tier metering, price discovery): add `meters.metering_only` as an **Orb-style feature** — a declared, info-level state that the guard permits — *when a customer actually needs a free meter*. Until then, the guard's known limitation is: you can't attach an intentionally-unpriced meter to a plan (fine at 0 customers).
- **Drift becomes real** (multiple live customers, operators editing rules under live subs): promote the loud log to the durable marker + banner.

## Alternatives considered (panel)

- **fail-closed / hold** — hold the whole invoice until a rule is bound. Rejected: unrated usage is *severable* (unlike whole-invoice `tax_calculation_failed`), so holding punishes provably-correct base+rated revenue; and the resolve-side auto-finalize gates key only on `TaxStatus`, so a create-time hold silently leaks on Stripe-Tax tenants.
- **durable-record marker** — the recording mechanism was strong (point-in-time JSONB, quantity-not-dollars), but as a standalone it drifts (live-context banner suppression flips warnings on historical invoices) and — post-verification — solves a problem (surfacing an intended-in-Orb/Lago state) no one else solves. Deferred.
- **config-guard (full)** — adopted in trimmed form (the plan-attach guard). Its extra rule-deletion drift guard is rejected as belt-and-suspenders (the loud log already makes drift non-silent).
- **annotate-only (marker + banner)** — deferred with durable-record for the same no-precedent reason.

## Consequences

- The reachable bug (attach an unpriced meter → silent $0 usage) is **prevented at authoring** with a clear error — Velox now matches how Stripe/Orb/Lago make pricing explicit.
- The one case the guard can't see (drift) is **no longer silent** (loud log), satisfying ADR-070 without an invoice-schema change.
- **Known limitation:** intentionally-unpriced (metering-only) meters can't be attached to a plan until `metering_only` ships. Acceptable at 0 customers; documented.
- **No** migration, no new invoice column, no attention-reason/banner, no frontend — the minimal, industry-aligned fix.

## Test coverage

`TestPlanAttachGuard_UnpricedMeter_ADR096`: CreatePlan rejects an unpriced meter; accepts a meter priced via a default binding; accepts a meter priced via a `meter_pricing_rules` row; UpdatePlan rejects adding an unpriced meter. The existing ADR-034 immutability tests keep passing (their meters are seeded priced). The engine loud-log sites are exercised by the existing billing suite (behavior-preserving — the money outcome is unchanged, only the log).

## Amendment 2026-10-14 (#981): refuse removing the last price, and count unpriced usage

Issue #981 named the drift this ADR deferred: a live plan's meter can lose
its price after the plan-attach guard ran. Two writes do it. One deletes
the meter's last pricing rule. The other clears its default binding (the
meter's default price). New model names are also routine in AI pricing: a
meter with rules for some models and no default leaves every new model
unpriced. And one invoice path was silent. A spend-threshold invoice
dropped unpriced usage with no log. On a meter with no pricing rules the
period close bills only from the threshold invoice on, so no log named
the usage before it. On a meter with rules the close's full-window pass
logged the whole period, so that usage was logged, but the usage after
the threshold invoice was logged twice.

**What other products do** (checked 2026-10-14; full notes in the velox-ops
research file `wrong-invoice-holes-981-980-design-2026-10-14.json`):

- All the peers checked build the invoice without the unmatched usage. None
  holds the invoice, refuses the event, or alerts. Lago bills it at a
  default price that is $0 unless set
  ([charges with filters](https://getlago.com/docs/guide/plans/charges/charges-with-filters)).
  Metronome shows it under Events with no charge
  ([quickstart](https://docs.metronome.com/guides/get-started/metronome-dashboard-quickstart)).
  Orb's dimensional pricing has no default and says to price every
  combination.
- Stripe refuses removing a price in use: "You can only delete prices that
  you've never used"
  ([manage prices](https://docs.stripe.com/products-prices/manage-prices)).
- Correction to the Context above: the Orb sentence "tracked but not
  billed" is no longer on the page this ADR cites.

**Decision.** Keep the peer norm: the invoice is built and sent without
the unpriced usage, and Velox does not bill it again later (the shape of
ADR-120). Close the two gaps:

1. **Guard.** Two writes are refused: `DeleteMeterPricingRule`, and
   `UpdateMeter` clearing the default binding. They are refused when the
   meter would have no pricing rule and no default binding while a plan
   wiring it has a live subscription. The answer is 409 `invalid_state`,
   the code the ADR-100 meter-binding guards use.
   "Live" is the set `CountLiveSubsByPlan` already uses for plan
   immutability: draft, trialing and active. This reverses the rejection
   above of a "rule-deletion drift guard". The named pressure is #981.
   It is a plain check, then the write, with no lock. A subscription created
   in between does not touch the meter row, so a meter lock would not order
   the two. Losing that race ends as an unguarded write would, and the
   signals below still report it. The guard covers the last price only.
   Deleting one of several rules can still leave one model unmatched; the
   signals report that too.
2. **Signals.** Each place that builds an invoice without unpriced usage
   logs a WARN with meter and quantity, and adds the quantity to
   `velox_usage_unpriced_quantity_total{meter_id, mode}` (`mode` is live
   or test; test-clock simulations are not counted, as in the other
   billing metrics). It counts after the commit that makes the invoice
   decision final. For a cancel, that is the caller's commit, so a cancel
   rolled back and retried by the billing run counts once. That covers the period
   close (with or without an invoice), the cancel final invoice (with or
   without one) and the spend-threshold invoice, which now logs too. Usage
   is counted once. A period close after a threshold invoice reports only
   on the pass that would have billed the bucket. Its full-window pass is
   for max and last buckets the threshold invoice left to it, so a sum
   bucket is not reported twice. The invoice preview warning now carries
   the quantity.

**Where the operator sees it:** the unmatched row of the customer usage
view (`GET /v1/customers/{id}/usage`, the "unmatched usage — not billed"
row on the customer page), the preview warning, the counter and the server
log. **How to bill it:** add a price (a pricing rule or a default price),
which later invoices use; bill past periods with a one-off invoice.

**Deferred** (in the velox-ops register, each with its trigger):

- A durable record of unbilled usage per closed period (meter, quantity,
  invoice). Rules are matched when usage is read, so after a price is
  added, the usage view shows a past period's usage as priced although its
  invoice did not bill it. Trigger: an operator needs past unbilled
  quantities after adding the price, or reports the usage view as wrong.
- A customer page card or attention banner for unbilled usage, and a $0
  invoice line for it. Trigger: an operator misses unpriced usage with the
  signals above.

