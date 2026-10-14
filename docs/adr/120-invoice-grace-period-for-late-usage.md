# ADR-120: A period's invoice is built after a grace period, and late usage after it is not billed automatically

**Date:** 2026-10-14
**Status:** Accepted

## Summary

Velox builds a billing period's invoice once `INVOICE_GRACE_PERIOD` has
passed after the period ends. The default is 1 hour, the range is 0 to 72
hours, and 0 builds it as soon as the period ends. Usage stamped inside the
period that arrives within the grace is on the invoice, unless a
spend-threshold invoice was built after its timestamp (see Risks).

Once the invoice is built it does not change. Usage for that period that
arrives later is stored but billed on no invoice, now or later; the operator
can bill it with a one-off invoice. Velox does not refuse late usage.

The grace delays every period-end invoice, flat-fee ones too, and the final
invoice of a cancel scheduled for a date. The invoice's dates and amounts do
not move; only when it is built, and what follows it, move later.

## Context

Usage reaches a billing system some time after it happens: pipelines batch,
retry and replay. Issue #961 asked what Velox does with usage that arrives
after its period has ended.

Before this change the answer was luck. The hourly billing run closed a
period on its first tick after the boundary, which could be one second after
it. A replica that booted or took over the leader lease closed every due
period on its first poll. `POST /v1/billing/run` closed at any instant, and a
catch-up after downtime closed the newest period with no delay at all. The
usage read happens at close time, so whatever had arrived by then was billed
and the rest was not.

#1038 and #1039 first made the gap between a period end and its invoice safe
to lengthen. A subscription change in the gap now closes the ended period
first. `last_ever` values are read as of the period end, and a pause is read
as it stood at each period end. This ADR then makes the gap a fixed setting.

What other billing products do (all quoted from their docs):

| Product | Grace before the invoice is built | After it is built |
|---|---|---|
| [Stripe](https://docs.stripe.com/billing/subscriptions/usage-based/configure-grace-period) | "All invoices have a default finalisation grace period of 1 hour." "You can set a delay of up to 72 hours (3 days)." | "Stripe might accept an event after finalisation and include it in meter summaries without updating the finalised invoice or adding the usage to the next invoice." |
| [Orb](https://docs.withorb.com/invoicing/invoice-issuance) | "12 hours after the invoice period ends" | "Once the invoice is issued, its values cannot be changed." |
| [Metronome](https://docs.metronome.com/invoice-customers/how-invoicing-works/) | "The grace period is 24 hours by default." "This buffer period allows for late-arriving usage data and invoice corrections." | "no further changes can be made, even if additional usage is reported for the period." |
| [Chargebee](https://apidocs.chargebee.com/docs/api/usages.md) | the invoice is `pending` until it closes | "If `usage_date` corresponds to a time already invoiced, then it is stored but never invoiced unless the invoice is regenerated." |
| [Lago](https://knowledge.getlago.com/articles/1455954942-what-happens-to-usage-events-sent-after-the-grace-period-ends) | a [grace period](https://getlago.com/docs/guide/invoicing/invoicing-settings/grace-period.md) in days, invoices stay draft | "Late events are still accepted by the API, but they won't appear on any invoice, past or future." (knowledge-base article, which calls itself not officially supported documentation) |

Kill Bill is the exception: it re-reads the past two periods and bills the
difference on a later invoice
([docs](https://docs.killbill.io/latest/userguide_subscription.html)).

## Decision

1. **Setting.** `INVOICE_GRACE_PERIOD`, a Go duration read once at start in
   `internal/config` (`loadInvoiceGracePeriod`). Unset is `1h`. Outside
   0 to 72h, or not a duration, start-up fails. It is passed to
   `api.NewServer` as `WithInvoiceGrace`, which threads the one value into
   the engine, the backlog gauges and the credit-expiry sweep.
2. **What waits.** A due instant is a period end (`next_billing_at`) or a
   mid-period `cancel_at`. The two wall-clock billing runs, the scheduler's
   `RunCycle` and `POST /v1/billing/run` (`RunCycleForTenant`), admit a
   subscription only when its due instant is at or before now less the
   grace. Both arms of both due scans use that instant
   (`GetDueBilling`, `GetDueBillingForTenant`: `now` and `dueBy`), and
   `billSubscription` uses the same rule for its cancel branch and its
   caught-up check. So a catch-up after downtime bills the older periods at
   once and the newest after its grace.
3. **What does not wait.** The billing-failure wait (ADR-119) compares with
   the real now. `BillDueNow` passes no grace; it is the close behind an
   immediate cancel, `retry-billing`, and the pre-close every other
   subscription write runs (#1038). The test-clock run passes no grace, so
   advancing a clock to a period end still produces its invoice. Spend
   threshold invoices and the final invoice of an immediate cancel are built
   at once, as before.
4. **Gauges.** `velox_billing_due_subscriptions` and
   `velox_billing_oldest_due_age_seconds` use the same shifted instant. The
   age is counted from the due instant plus the grace.
5. **Credit expiry.** The wall-clock sweep retires a grant once the grace
   has passed after its `expires_at`. The test-clock sweep is unchanged.
6. **Dates.** `billOnePeriod` still runs at the due instant: the invoice's
   period, `issued_at` and `due_at` stay at the boundary, and a scheduled
   cancel is stamped at its `cancel_at`.

## Why this design

**One grace on the due instant, nothing else.** The close already reads
usage by event timestamp at close time. Delaying the close is the whole
mechanism: no draft that is re-aggregated, no second read. A draft that is
refreshed was the alternative; Velox's draft lines are frozen once written,
so it would have needed a new re-aggregation path.

**Both scan arms and the engine use one instant.** If only the
`next_billing_at` arm waited, a row admitted by the `cancel_at` arm would be
declined by the engine, and the drain would save `errStillDue` as a billing
failure on a healthy subscription. The tests check that no failure is saved
inside the grace.

**The manual run waits.** `POST /v1/billing/run` is a due scan. If it did not
wait, one call would close every period inside its grace and cut off the
usage still arriving.

**Operator actions on one subscription do not wait.** An operator who cancels
or changes a subscription, or retries its billing, is acting now. Making them
wait would refuse the cancel with `unbilled_period` for up to the grace.

**Test clocks do not wait.** In a simulation the operator decides when usage
arrives relative to an advance, and every existing clock flow expects the
invoice at the boundary.

**The credit sweep waits.** The close draws on grants that were live at the
period end. The sweep runs every tick, after the billing run. Before, the
first tick after a boundary closed the period before it swept, so a grant
expiring just after the boundary still paid that period. With a grace longer
than the tick, a tick inside the grace would retire the grant first, and the
customer would lose credit that was live when the period ended.

**1 hour by default.** It is Stripe's default and the short end of the
peers (Orb 12h, Metronome 24h). Today's hourly tick already delayed a close
by up to an hour, so 1h changes the least for an upgrade. An operator whose
pipeline lags longer raises it.

## Alternatives considered

- **A. Bill late usage on a later invoice (Kill Bill shape).** Rejected by
  the decided policy. It re-prices non-additive and tiered meters as
  differences, taxes old usage at the new invoice's date, and turns a resent
  event without an idempotency key into a direct double charge.
- **B. Refuse events older than a window (Stripe 35 days, Orb about 12 hours
  at ingestion).** Not now: refusing breaks retries and pipelines, and it
  does not bill anything that is refused. Deferred until abuse or volume
  asks for it.
- **C. Name it `USAGE_GRACE_PERIOD`.** Rejected: it delays every
  period-end invoice, including flat-fee ones, with their webhook, tax
  record, credits, charge and dunning start. Stripe calls it a finalization
  grace for all invoices too.
- **D. A per-tenant setting.** Deferred: Orb and Metronome change it only
  through support. Trigger: a tenant asks.

## Consequences

### Positive

- Usage that arrives within the grace is billed, whatever the tick phase,
  failover or restart. The one exception is in Risks: usage stamped before a
  spend-threshold invoice.
- The policy is written down and the same for every peer-shaped question:
  what counts, when the invoice is built, what happens after.

### Risks / open items

- **Everything after the invoice moves later by up to the grace:** the
  `invoice.finalized` webhook, the tax record, credits applied, the charge
  and the start of dunning. With net-0 terms an invoice is past due when it
  is created, as it already was by up to an hour.
- **Scheduled changes take effect at the close, too.** A cancel or plan
  change scheduled for a date, and a scheduled resume of paused collection
  that falls after a period end, wait for that close. Until then the
  subscription is `active` on its old plan, and `subscription.canceled` is
  sent at the close. `canceled_at` stays the scheduled date. An integration
  that gives access while `status` is `active` gives up to the grace (at
  most 72h) more.
- **Usage stamped before a spend-threshold invoice and arriving after it is
  billed on no invoice**, even inside the period. The period-end invoice
  bills usage only from the threshold invoice's end on
  (`meterUsageIntervals`). This was already so before the grace. Bill it
  with a one-off invoice. With `reset_billing_cycle`, the threshold invoice
  closes the period at once.
- **The subscription shows the ended period during the grace.** Spend
  thresholds do not fire on the new period's usage, and the live credit
  balance leaves it out, until the invoice is built. A grant created during
  the grace is drawn by the ended period's invoice. Deferred until a
  spend-cap complaint.
- **An expired grant stays in the ledger balance for up to the grace
  longer.** No later invoice draws from it, because the draw filters on the
  invoice instant.
- **The sweep and the close read "now" at different instants in one tick.**
  A grant can still be retired before a close if the drain takes longer
  than the gap between them. This was already possible with no grace.
- **Late usage after the invoice is silent.** There is no alert and no list
  of the events that missed an invoice. `velox_usage_late_event_total`
  counts events more than 24 hours old on arrival, which is a different
  thing. **Deferred: an exact per-customer report** of events that arrived
  after their period's invoice was built (design in velox-ops,
  `research/late-usage-961-design-round2-2026-10-14.json`). Trigger: an
  operator or a customer needs to know which events missed an invoice, and
  comparing the customer's usage with the invoice lines is not enough.
- **Walks and scripts** that close a wall-clock period with
  `POST /v1/billing/run` right after it ends must run with
  `INVOICE_GRACE_PERIOD=0`.

## References

- ADR-028 (test clocks and wall-clock runs are separate), ADR-097 (mid-period
  scheduled cancel), ADR-115 (one closer per period), ADR-119 (billing
  failure record).
- #961 (late usage), #1038 and #1039 (writes and pauses in the gap).
- `internal/config/config.go` `loadInvoiceGracePeriod`;
  `internal/billing/engine.go` `SetInvoiceGrace`, `RunCycle`,
  `RunCycleForTenant`, `billSubscription`;
  `internal/subscription/postgres.go` `GetDueBilling`,
  `GetDueBillingForTenant`; `internal/api/middleware/metrics.go`;
  `internal/credit/postgres.go` `ListExpiredGrants`.
