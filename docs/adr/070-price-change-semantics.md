# ADR-070: Price-change semantics — overrides follow rule_key, periods pin at open

**Date:** 2026-07-02 (decisions locked Day 1); **amended 2026-07-03 at ship time** — §4 resolver mechanics and §Consequences updated to what shipped (implementation = migration 0128 + resolveRatedRule)
**Status:** Accepted & shipped (designed by the P4 adversarial panel — 3 lenses, all SHIP-WITH-FIXES; consolidated protocol in plan §4.4) (amended 2026-07-03, on 2026-07-26 by [ADR-100](100-currency-coherence-guard-ring.md), and on 2026-10-23: [meter pricing edits](#amendment-2026-10-23-meter-pricing-edits-are-refused-while-a-subscription-uses-the-meter)); superseded in part by [ADR-130](130-usage-prices-belong-to-the-plan.md) (parts of decisions 1, 4 and 5, and the meter-in-use amendment), built in PR-4 (#1386)

**Superseded in part by [ADR-130](130-usage-prices-belong-to-the-plan.md)**, decided 2026-10-23 and built in PR-4 (#1386). Where this ADR differs, ADR-130 describes the code. Decision 1 changes, because an override keys on (customer, price) instead of `rule_key`. Decision 4 changes: versions are internal plan-price versions, and a price added to a used plan bills from the next period (no earliest-version fallback). Its test-clock caveat is withdrawn, because every price resolves in the subscription's own time. Decision 5's currency guard goes, because a price has no currency. The 2026-10-23 meter-in-use amendment is replaced. Its "Temporary" note names version 0.10; the change now lands in v0.9.0. Decisions 2, 3 and 6 stay: a period is priced at its start, every rate writer follows that rule, and a failed lookup stops the close. One exception to decision 2: customer usage over an explicit `?from=&to=` window becomes an estimate, priced at each subscription's current-period pin, not at each past period's own pin.

## Summary

The rating-rule version in force when a billing period opened prices that whole period, so a price change never reprices usage already metered. New rule versions and changes to customer price overrides take effect at the next period open, and overrides are keyed by `rule_key`, so a negotiated price survives a new version. A failed price or override lookup stops the close or preview with an error; it never bills list price instead. A republish that changes a rule's currency is refused while active overrides use the rule or a plan in another currency bills it. Amended 2026-10-23: the meter's own pricing (its pricing rules, default price and aggregation) is read at billing time, so editing it would reprice the open period. While a draft, trialing or active subscription uses the meter, such an edit is refused with 409 `meter_in_use`; a new version is the way to change a rate. Status: active, amended 2026-07-03, 2026-07-26 ([ADR-100](100-currency-coherence-guard-ring.md) added the plan check) and 2026-10-23.

## Context

Two product decisions gated P4 and P10:

1. **Override keying.** Customer price overrides are stored and looked up by
   `rating_rule_version_id`, but the single-rule cycle close resolves
   `GetLatestRuleByKey` and then fetches the override by the *latest* version
   id. Publishing v2 of a rule makes every v1-keyed override unfindable: the
   negotiated customer silently reverts to list price for the whole period at
   the next close — no event, no warning (audit High #3).
2. **Repricing semantics.** Nothing defined what a mid-period rate change
   *means*. Worse, resolution is split-brain today: cycle close prices at
   latest-by-key, while preview, the threshold scan, and customer_usage price
   at the meter-pinned version — so one period can bill at two different
   rates (threshold fires at v1, close bills the residual at v2), and
   preview ≠ invoice by construction.

## Decisions

1. **Overrides are keyed by `rule_key`** — a negotiated price follows the
   rule across version publishes. `rule_key` column added and backfilled via
   the rating_rule_versions join; `UNIQUE (tenant_id, customer_id, rule_key)`
   replaces the version-id unique. The backfill collapses collisions
   deterministically (an operator who re-created an override after a publish
   holds rows against v1 AND v2): highest rule version wins, tie-break latest
   `updated_at`; losers flip `active=false` and are kept for audit, each
   demotion logged. The API keeps accepting `rating_rule_version_id` and
   resolves it to `rule_key` at the service boundary. Copy-forward
   (re-writing overrides onto each new version) is rejected: it multiplies
   writers and re-creates the detach bug on every miss.
2. **Repricing is PINNED, never retroactive: resolve-as-of-period-start.**
   The rating-rule version in force when a billing period *opened* prices
   the entire period, everywhere — cycle close (single-rule and multi-dim),
   both cancel paths, preview, threshold fires, customer_usage. A publish
   takes effect at the next period open. Already-consumed usage is never
   repriced by a catalog change. One shared resolver; the split-brain
   pinned-vs-latest mix is removed, which also makes mid-close publishes
   harmless (resolution becomes time-invariant for the period — no
   snapshot-consistency locking needed).
3. **The same effective-next-period rule binds ALL rate-state writers** —
   version publish, override upsert, override deactivate. If version
   publishes were next-period but override edits stayed
   retroactive-at-close, the spend-cap coherence argument collapses (the
   threshold scan compares `capRunning` at old rates, close bills at new).
4. **Resolver mechanics (as shipped):** as-of resolution keys on
   `created_at` — now stamped via `clock.Now(ctx)` instead of wall-clock
   (no separate `effective_from` column; a version's creation IS its
   effectivity, and versions are immutable). The resolver
   (`GetRuleByKeyAsOf` / engine `resolveRatedRule`) picks the highest
   ACTIVE version with `created_at <= periodStart`; archived/draft
   versions never resolve. **A key born mid-period resolves to its
   earliest active version** — a rule created after the period opened
   has no prior price to preserve, and refusing to bill would block
   every close for the common onboarding shape (meter + rule created
   mid-month). Overrides get NO such fallback: an override absent at
   period open means list price (a fine default exists). Overrides are
   append-only effectivity rows — an upsert closes the prior row's
   window (`deactivated_at`) and inserts a fresh one, so the in-flight
   period keeps resolving the row it opened with. Test-clock caveat:
   forward-advancing clocks (the Stripe shape — created at now, advanced
   forward) resolve coherently because sim time runs ahead of the
   wall-clock stamps; a clock frozen in the PAST would resolve
   publishes made "during" the simulation as future — accepted, since
   Velox clocks, like Stripe's, only advance forward from creation.
5. **An override freezes PRICE, not rule semantics.** `ToRatingRule` is
   replaced by patch-the-resolved-rule: the resolved version's `ID`,
   `RuleKey`, `Currency`, `Name` survive; only pricing fields
   (mode/tiers/amounts) are swapped. This differs from Lago's full-immunity
   model (subscription immune to all plan edits) deliberately: non-pricing
   semantics of new versions still flow to overridden customers. Corollary
   guard: a publish that *changes currency* within a rule_key is rejected
   while active overrides reference that key — a rule_key-following override
   would otherwise silently reinterpret its integer cents in the new
   currency.

   **Correction (2026-10-23):** Lago has no full-immunity model. Lago edits
   plans in place, so base-fee and charge-price edits reach live
   subscriptions. A premium per-subscription override (`plan_overrides`,
   a child plan) keeps one customer apart only in part: a plan edit sent
   with `cascade_updates` also changes each child plan that still has the
   old base fee or charge prices. Checked in lago-api `2e52dfc`
   (2026-10-22): `app/services/plans/update_service.rb:32-85` and
   `:151-190`. See the
   correction in [ADR-034](034-plan-billing-field-immutability.md#industry-reference).
6. **Resolution failures are loud.** `errs.ErrNotFound` from the override
   lookup means "no override — list price". Any other error aborts the
   close/preview and is returned; today all five engine lookup sites treat
   any error as "no override" and a DB blip silently bills list price
   (violates no-silent-fallbacks).
7. **Pin-at-consumption is REJECTED**, not deferred: per-event version
   stamping is a heavier model (per-event rate joins, unbounded backfill
   semantics) that no current customer shape demands; period-start grain is
   the v1 semantics. Nothing spills to backlog from this decision.

## Consequences

- P10's `customer_usage.go:401` slice implements override lookup by
  `rule_key` with the same as-of-period-start resolution, citing this ADR.
- Version allocation moved into SQL (`MAX(version)+1` in the INSERT;
  service retries the rare 23505) so concurrent publishes stop 409ing
  spuriously.
- **Recipe reinstall ADOPTS the existing graph by natural key** (rating
  rules by `rule_key`, meters by key, plans by code) instead of
  recreating or 409ing — Uninstall keeps the objects because the
  operator owns them, so reinstall reconnects and never clobbers
  post-uninstall operator edits (adopting also avoids a reinstall
  silently REPRICING live subs, which version-bumping would). Residual:
  a recipe-created webhook endpoint has no natural key and would
  duplicate on reinstall — named backlog item.
- Mid-period override deactivation (new DELETE endpoint) takes effect at
  the next period open, same as every other rate change; the row is kept
  with its window closed for audit and historical resolution.
- Migration 0128 upgrade note: an override created mid-period under the
  OLD semantics applied to that period's close; post-0128 it prices from
  the next period open (its `created_at` postdates the period start).
  Pre-launch, zero production tenants — accepted without backfill.
- Full test matrix in plan §4.4 item 11, all mutation-verified 2026-07-03
  (4 mutations killed: version-id override keying, latest-not-period-open
  resolution, fabricated ApplyTo dropping Currency, restored error
  swallow).

## Amendment 2026-10-23: meter pricing edits are refused while a subscription uses the meter

Decision 2 says a catalog change never reprices usage already metered. That
held for rule versions and overrides, but not for the meter's own pricing.
Billing reads the meter as it is at close, preview or threshold time: its
pricing rules (which rule claims which event), its default price and its
aggregation. So these edits repriced the period already open, with no
check (D05 in the velox-ops strategy of 2026-10-23):

- adding, editing or deleting a pricing rule;
- pointing the default price at another rule, or removing it;
- changing the aggregation.

**Decision.** While a draft, trialing or active subscription uses the meter,
these edits are refused with 409 `meter_in_use`. The message gives the
number of subscriptions and points to publishing a new version, which waits
for the next period. On a test clock moved forward, a version published now
can apply to the period already open (the test-clock caveat in decision 4),
so in test mode the message says so. A subscription uses the meter when an item is on a plan
that lists it. It also uses it when an item moved to or from such a plan
during the current period, because the close bills the meter up to the move.

Edits that change no price already billed are allowed:

- a first default price;
- a new pricing rule on a meter with no default price, when every rule that
  could match the same events has a higher priority. It prices only usage
  no rule prices now, which is how a new model gets a price;
- the default pointed at another version of the same rating rule;
- a write of the values the meter already has.

The first two do change what a subscription pays: usage that had no price,
including usage already recorded this period, is then billed. One exception:
if a spend threshold already invoiced the subscription this period, a sum or
count price bills only the usage after that invoice. The usage before it
stays unbilled, as the threshold invoice left it out
([ADR-096](096-unpriced-meter-plan-attach-guard.md)). A max or last price
is billed over the whole period.

The check runs in the edit's transaction. It locks the meter row, then
every plan that lists the meter, then counts. A subscription item insert or
plan change checks its foreign key with a share lock on the plan row. So a
subscription being created on such a plan and the edit wait for each other
instead of interleaving. The guard replaces the narrower last-price guard of
[ADR-096](096-unpriced-meter-plan-attach-guard.md#amendment-2026-10-14-981-refuse-removing-the-last-price-and-count-unpriced-usage).

**Not covered.** A plan that gains the meter (created with it, or attached
to it) while a subscription is created on that plan, all between the check
and the edit's commit, is not ordered. That subscription's period is priced
as if the edit came first. On a meter with a default price, no new model can
be priced while subscriptions use it.

**Temporary.** Version 0.10 moves usage prices to the plan and makes every
pricing edit wait for the next period (D08). This guard then goes.
