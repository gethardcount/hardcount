# ADR-066: Threshold money semantics — prorated reset base, atomic fire→reset, $0 auto-pay, max/last deferral

**Date:** 2026-07-02
**Status:** Accepted (fixes 4/atomicity/6 shipped in P1b #339; fix 5 + the immediate-cancel watermark shipped in P1c)

## Context

ADR-065 fixed the threshold-scan *kernel* (boundary-skip, fire-once probe,
cursor-drain). This ADR records the *money semantics* of the remaining
P1 defects, as decided by the build-time adversarial panel (three lenses:
money-correctness/accounting-identity, concurrency/crash/retry,
operator-semantics/industry-parity — all returned SHIP-WITH-FIXES and
materially amended the plan's §4.1 sketch; plan doc updated same-day).

## Decisions

### 1. reset=true fires bill a PRORATED in_arrears base (shipped, P1b)

With `reset_billing_cycle=true` the fire re-anchors the cycle at the fire
instant, so the cycle close never true-ups the fired window's base — the
threshold invoice IS that window's base bill. It used to carry the FULL
month's base: a sub crossing 3×/month paid base 3×.

**The denominator is the plan's own full interval, never the current period
length.** The panel rejected the plan's sketched
`roundDays(periodEnd−periodStart)`: it violates the codebase's proration
invariant (`domain/subscription.go` — divide by the FULL cycle length),
over-bills re-anchored stubs (+43% at one observed point), mid-cycle-created
periods (up to ~2×), cross-interval items (~12× for a yearly-interval line on
a monthly-period sub), and divides by zero on a sub-day stub. The shipped
formula mirrors `emitBaseSegmentLine` per line:

```
fullCycleDays = roundDays(advanceBillingPeriod(periodStart, linePlan.BillingInterval, loc, anchorDay) − periodStart)
segDays       = roundDays(now − periodStart)          // capped at fullCycleDays
prorated      = RoundHalfToEven(base × qty × segDays, fullCycleDays)
```

The whole line triple is rewritten — description gains the standard
`(prorated X/Y days)` suffix and the unit amount is recomputed — so
`qty × unit == amount` on every render surface. The prorated amount feeds the
`amount_gte` running total (the invoice matches the cap comparison).
`reset=false` behavior is byte-identical (the cycle close bills the residual
and skips base behind the watermark — full base on the fire stays correct).

**Accepted conventions:** day granularity (a sub-day window prorates to a $0
base stub, matching the engine-wide `roundDays` convention); across N fires
the independently-rounded numerators may drift the cycle's total base by up
to ±1 day's base per fire boundary; `POST /v1/invoices/create_preview` is
untouched (public surface, and the reset=false close-path base-skip depends
on it) — a preview over a reset=true sub shows the full base while the fire
would bill prorated. Documented divergence, same shape as Stripe's upcoming-
invoice preview not modeling threshold fires.

### 2. Fire→reset is ONE transaction (shipped, P1b)

`fireThreshold` committed the invoice, then ran tax-commit / credits /
charge / events, and only then `UpdateBillingCycle` — with the failure arm
logging and returning success under a "next tick reconciles" comment that
was false: the fire-once probe finds the committed invoice and skips every
later tick, so a crash or transient failure between the writes stranded the
reset FOREVER. Under decision 1 that degrades to permanent base
under-billing (prorated base on the invoice, full base skip at close).

Now the insert and re-anchor commit atomically via the engine's `TxRunner`
seam (`postgres.DB.WithTenantTx`; both stores' `…Tx` variants already
existed). Failure rolls both back — the probe stays clear and the next tick
retries the whole fire. External calls stay post-commit (the tx never spans
network I/O — deliberate; see the batch-claim design discussion in ADR-065's
lineage). A missing TxRunner on a reset fire fails loudly; there is no
sequential fallback.

### 3. $0 invoices auto-mark paid, both writers + heal (shipped, P1b)

The zero-due MarkPaid gates in `fireThreshold` AND `billOnePeriod` required
`totalWithTax > 0`, stranding invoices born $0 (zero-priced usage lines
crossing a `usage_gte` cap; free-rated plans) as payment_pending forever —
never charged, never paid, permanently "awaiting payment" in the attention
queue. Both gates are now `creditApplyOK && status == finalized`. Stripe
parity verified by the panel: zero-amount invoices auto-mark paid with no
payment attempt; `invoice.paid` fires (consumers should read it as
"settled", not "money received"). Tax-pending and pause-collection drafts
remain excluded by the status gate.

**Heal-on-re-entry:** a crash between create and MarkPaid re-enters only
through the fire-once probe (threshold) or the ErrAlreadyExists branch
(cycle) — neither reached the gate again. Both paths now run an idempotent
`healStrandedZeroDue` (probe fetches the full row via
`GetLatestThresholdInvoiceForCycle`; the cycle branch via
`GetInvoiceForPeriod`, the exact billing-idempotency tuple). Best-effort by
design: a heal failure WARNs and leaves the operator-visible pending row
rather than blocking the cycle advance — stalling all future billing over a
$0 paid-mark is the worse trade.

### 4. max/last split-billing (shipped, P1c)

Non-additive aggregations (`max`, `last_during_period`, `last_ever` — the
original plan forgot `last_ever`) cannot be split across a threshold fire and
a cycle close: `max[0,t1) + max[t1,end) ≥ max[0,end)`. Decisions, recorded
now so P1c implements against them without re-litigating:

- **Exclusion iff `reset=false`** (locked earlier): drop non-additive lines
  from the threshold invoice; the cycle close bills them full-period exactly
  once. Under `reset=true` they ride the fire (the re-anchored stub never
  gets a close for the pre-fire window — excluding would bill them by
  NOBODY).
- **Granularity is the RULE BUCKET, not the meter.** One meter can carry sum
  and max rules simultaneously (`meter_pricing_rules.aggregation_mode`);
  meter-level classification preserves the exact split-billing bug on mixed
  meters. AggregationMode gets plumbed onto PreviewLine.
- **The close-side clamp exemption keys on INVOICE GROUND TRUTH** — "the
  watermark threshold invoice carries no line for this bucket" — never on
  `sub.BillingThresholds.ResetBillingCycle` at close time: the flag is
  operator-mutable between fire and close (a PATCH flip would resurrect the
  billed-by-nobody gap), and a stranded pre-ADR-066 reset makes it a lie.
- **Panel question (a): max/last amounts count toward `amount_gte` iff
  `reset=false`.** Asymmetric on purpose. Under reset=false the cap measures
  committed spend (for the AI-infra segment, max-metered GPU concurrency can
  be MOST of the spend) and the probe guarantees at most one fire per cycle.
  Under reset=true, counting them creates a runaway refire loop — a steady
  peak re-materializes in every re-anchored window: cross → fire + card
  charge → re-anchor → cross again, one invoice per scheduler tick.
- **Panel question (b): a pure-max/last crossing SKIPS** (no invoice — the
  billable set after exclusion is empty), with a loudness floor: the
  empty-set guard sits before any invoice-number mint or paid tax call, and
  emits a once-per-(sub, period) audit/timeline artifact + deduped WARN, not
  tick-spam. Residual-subtraction was rejected by all three lenses:
  meaningless for `last` (a point-in-time read is not subtractable),
  over-bills `max` when the peak straddles the fire, and produces an invoice
  no auditor can recompute. PATCH-time steering (warn when configuring
  `amount_gte` on a pure-max/last sub) is deferred — cross-domain seam;
  trigger = first operator confusion report.

### 5. Named site-set discoveries (panel), disposition

- **Immediate-cancel final bill ignores the threshold watermark** — a second
  period-closer the original design missed: after a reset=false fire, an
  immediate cancel double-billed the pre-fire sum usage + base. FIXED in
  P1c: the watermark protocol (base skip + additive clamp + non-additive
  exemption) lives in one shared helper (`thresholdWatermark`) consumed by
  both `billOnePeriod` and the cancel path.
- **`{in_advance base × reset=true}` under-bills** one base slice per fire
  on anniversary subs (the re-anchored period's pre-existing-prepaid window
  is never re-billed; calendar subs snap back and are unaffected).
  Customer-favored, rare, pre-existing. DOCUMENTED, not fixed in-window;
  trigger to revisit = first DP configuring reset=true on in_advance plans.

## Test locks (mutation- or fault-injection-verified)

Proration: `TestEvaluateThresholds_ResetProratesBase` (+ full-line-triple
assertions), `…_StubPeriodDenominator` (2333-vs-1633 seam),
`…_CrossIntervalDenominator` (10950-vs-900 seam),
`TestThresholdFire_ResetProratesBase_RealStore` (real preview path + persisted
lines + atomic re-anchor). Atomicity:
`TestThresholdFire_ResetAtomic_RollsBackOnAdvanceFailure` (real-PG rollback +
clean retry), `TestScanThresholds_ResetAdvanceFailure_IsLoud`,
`…_ResetWithoutTxRunner_FailsLoud`. $0: `TestFireThreshold_ZeroTotal_AutoPaid`,
`TestBillOnePeriod_ZeroTotal_AutoPaid`, both heal tests. The multi-fire
base-identity test (N prorated fires + close == exactly one base) needs
simulated time across re-anchors — lands with P1c's clock-driven suite.

## Amendment 2026-10-11: tiers continue across the fire (P21)

Under `reset=false` the fire bills `[periodStart, T)` and the close bills
`[T, periodEnd)`. Each side rated its slice from tier 0, so a graduated or
package rule restarted its ladder at the fire and the period overbilled. For
example, a ladder of 1,000 units at 10¢ then 2¢, with 700 units before the
fire and 600 after, billed $70 + $60. Pricing the whole period gives $70 +
$36. Stripe, OpenMeter (#4298 fixed the same bug), Lago and Kill Bill all keep
one ladder across a mid-period threshold invoice. Cycle resets
(`reset=true`) still restart tiers, as they restart the cycle.

**Decision: marginal pricing per additive bucket, seeded from the fire.**
The closers (cycle close and immediate cancel, multi-dim and single-rule)
keep one ladder per bucket. The seed is the quantity on the fire's usage
line for the same (meter, resolved rating rule version). Each later slice
bills `max(0, F(quantity so far + q) − F billed so far)`, where the seed's
share is F(seed) recomputed under the rule.

- **Exact for graduated.** The fire and the close sum to F(total).
- **The seed amount is recomputed, never read from the line.** In
  tax-inclusive mode `ApplyTaxToLineItems` rewrites the line's AmountCents to
  the net amount. Subtracting that from a gross F would bill the fire's tax
  again (found by review: 20% inclusive VAT overbilled by exactly the fire's
  tax).
- **Flat rules keep per-slice pricing.** Flat pricing is linear, so the seed
  changes nothing. Leaving flat untouched keeps it exact, as it already was.
- **A usage cap turns the ladder off.** The fire is uncapped, while the
  close scales its quantities by the cap. Continuing an uncapped seed would
  price the capped units at the top tier, making the existing cap+threshold
  gap worse. Capped closes price as before.
- **Package pricing can be non-monotone.** Nothing checks
  `overage × size ≤ package price`, so a larger quantity can cost less. A
  slice is then clamped at 0, and the period bills max(F(fire), F(total)).
  Tracking the billed amount, not just quantity, lets a later slice in the
  same close recover the difference.
- **The key is the resolved version.** The close resolves the rule
  (`resolveRatedRule` at period start, as the fire does) and looks the seed
  up by the resolved id. A version published before the period opens leaves
  the binding on the old id while both invoices carry the new one, so a key
  on the binding would silently restart tiers.
- **Ambiguous configs fail loudly** (graduated and package only). Lines carry no pricing-rule id. So the
  seed lookup is ambiguous when two buckets of one meter resolve to the same
  version. That happens with the meter's default binding plus a pricing rule
  pinned to it, or with two pricing rules pinned to versions of one rule
  key. When that happens
  and the fire billed that version, the close returns an error naming the
  meter and version, and retries each tick. The check reads the
  configuration, not which buckets happen to have usage, because a bucket
  with no post-fire usage would otherwise hand its seed to another.
- **Non-additive buckets are unchanged.** Under `reset=false` the fire drops
  them (§4). Under `reset=true` the fire is outside the closer's window.
- **No fire, no change.** Disjoint windows of one meter in a cycle (an item
  removed and re-added) still rate each window from tier 0. Velox models the
  re-add as a separate item, and Stripe gives each item its own ladder.
  Changing that is a separate decision.

**Out of scope (registered):**
- Usage caps with thresholds: the fire is uncapped while the close is
  cap-scaled over the whole period.
- An authoring guard against the ambiguous configs.
- Package monotonicity validation.
