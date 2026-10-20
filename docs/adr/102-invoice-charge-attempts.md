# ADR-102: Charge attempts as first-class facts

Date: 2026-07-28
Status: Accepted (partly superseded by [ADR-103](103-single-payment-source.md) on 2026-07-28 and [ADR-104](104-one-activity-lane-entity-calendar.md) on 2026-07-29: the rendering rules; amended 2026-10-19 by [ADR-128](128-activity-line-standard.md): hosted-page attempts)

## Summary

Each charge attempt is one row in `invoice_charge_attempts`, created when Velox creates the PaymentIntent and updated by PaymentIntent id as Stripe reports the outcome. The row records what triggered the attempt (`auto_charge`, `charge_now` or `dunning_retry`; see the 2026-10-20 amendment) and its test-clock time, and a `succeeded` outcome is final. Before this, a failed charge reached the invoice timeline only through a Stripe webhook row or a dunning (payment retry) row, so a simulated invoice with dunning off could hide it. [ADR-103](103-single-payment-source.md) moved the outcome write into the settle transaction and made these rows the only source of payment rows on the timeline. Status: active, partly superseded by ADR-103 and by [ADR-104](104-one-activity-lane-entity-calendar.md), which removed the separate real-time timeline card. [ADR-128](128-activity-line-standard.md) reverses one rule below: an attempt made on the hosted payment page is to be dated on the invoice's clock, not on real time. Until that change ships, it is still dated on real time.

## Context

A charge attempt is a billing fact — the engine tried to collect an
invoice at some instant and the attempt had an outcome — but it had no
record of its own. Its timeline representation rode on whichever
neighbor happened to exist:

- the **Stripe webhook row** (`payment_intent.payment_failed` /
  `succeeded`) — a wall-clock fact, exiled to the wall-clock lane on
  simulated invoices because its timestamp can't sort into a
  simulated sequence;
- the **dunning-started row** — anchored on the billing axis via
  `simulatedFailureAt`, but an event of an *optional* subsystem.

Consequence, found by the I5 dunning-disabled walk (2026-07-28): on a
simulated invoice with dunning off, **both** carriers are absent from
the billing axis and the Activity timeline goes silent about a failed
charge. The invoice's own story changed shape based on a collections
setting. Related smaller gaps: a lost webhook left auto-charge failures
with no timeline row at all, and a PaymentIntent-create failure
(network error — no PI, no webhook, ever) was never representable.

## Decision

**One table, `invoice_charge_attempts`: one row per charge attempt,
upserted by PaymentIntent id as provider truth arrives.**

Writers (all best-effort — see Consequences):

1. **The saved-PM charge chokepoint** (`chargeInvoice`) inserts the row
   when it creates the PI — it is the only writer that knows the
   `trigger_source` (`auto_charge` | `dunning_retry` via the PI
   purpose) and the **sim anchor** (`clock.SimOf(ctx)` during a
   test-clock Advance). Outcome at insert: `pending` (PI created),
   `failed`/`unknown` (create-time decline/ambiguity — including the
   empty-PI shape, whose row is the attempt's *only* record).
2. **The settle paths** (`SettleSucceeded` / `SettleFailed` — webhook,
   inline sync-success, reconciler) upsert the outcome by PI. For a PI
   Velox didn't mint inline (hosted checkout) this insert IS the
   record, `trigger_source='external'`, deliberately wall-clock — an
   interactive act is a wall fact.

> Reversed 2026-10-19 by [ADR-128](128-activity-line-standard.md): see
> [Amendment 2026-10-19](#amendment-2026-10-19-hosted-page-attempts-follow-the-invoices-clock).
> Until that change ships, this paragraph describes the code.

Upsert semantics (enforced in SQL): `succeeded` is terminal; every
other outcome may advance — including `failed → succeeded` (a 3DS PI
retried within one checkout session). Insert-time identity
(`trigger_source`, `sim_effective_at`) is never overwritten by a
settle upsert. Partial UNIQUE on `(tenant_id, stripe_payment_intent_id)
WHERE pi <> ''`; empty-PI attempts are insert-only.

**Rendering: every attempt appears exactly once, via the richest owner
available — dunning row → attempt row → stripe webhook row.**

- A PI carried by a **dunning row** (the walked #639/#640 fold design)
  renders there; the attempt is suppressed.
- Otherwise the **attempt row** renders — on the billing axis
  (`sim_effective_at`) when sim-stamped, closing the dunning-off gap;
  wall-stamped attempts join the wall-clock lane. On simulated
  invoices a sim-stamped attempt **replaces** its surviving webhook
  echo (lifting the echo's folded email Detail + provider error); on
  wall-clock invoices the webhook row already sits in the right time
  domain and the attempt defers to it — zero rendering churn.
- A `succeeded` attempt defers to the `invoice.paid` lifecycle row;
  on a NOT-paid invoice it renders ("Payment collected") — that shape
  is an anomaly worth surfacing. `pending` never renders (the
  attention banner owns in-flight).

**No backfill.** Invoices predating the table have no attempt rows, so
the precedence chain bottoms out at the webhook row and they render
exactly as before. The fallback tier is permanent, not transitional —
it also covers any future lost attempt write.

## Alternatives rejected

- **Synthesize a failure row from invoice state** (`payment_status` +
  `simulatedFailureAt`): no new storage, but state-derived — the row
  vanishes when the invoice is later paid, mislabels interactive
  declines on dunning-off simulated invoices with the cycle-close
  instant, and represents only the *latest* attempt. A patch over the
  same modeling gap.
- **Enroll dunning always, act conditionally**: makes runs that aren't
  campaigns; contaminates dunning's semantics to solve a display
  problem.
- **Route webhook rows into Activity on simulated invoices**: puts
  wall-clock stamps into a simulated sequence — the exact bug class
  the timeline-surfaces audit fixed for credit notes.

## Consequences

- The attempt write is **best-effort, not in-tx** with
  `UpdatePayment`: a crash between them loses only the attempt row,
  and the renderer falls back to the webhook row — display
  degradation, never money. This is the justified-dual-write shape
  (2026-06-24 audit): the fallback chain is the reconciliation.
- `invoice_charge_attempts` cascades on invoice delete (ADR-086
  test-clock teardown).
- The second invoice-page card has one constant identity — "Real-time
  activity" — instead of renaming itself between "Notifications" and
  "Real-time activity" as folds consume rows (2026-07-28 design
  review; one caption covers emails + payment events and points to the
  customer page for dunning reminders).
- Future surface: `trigger_source` gives an exact per-attempt
  provenance if payment-attempt history becomes a product surface —
  without re-deriving it from PI metadata.

## Amendment 2026-10-19: hosted-page attempts follow the invoice's clock

**Change.** Writer 2 above inserts the attempt for a payment made on the
hosted payment page (`trigger_source='external'`) with no test-clock time,
"deliberately wall-clock", because "an interactive act is a wall fact".
[ADR-128](128-activity-line-standard.md) reverses that rule. When the settle
path writes the attempt, it stamps `sim_effective_at` from the invoice's
clock, the same as any other act on a pinned invoice. The owner approved it
on 2026-10-18 as decision 7 of the activity line standard.

**Why.** A customer paying is an act on the invoice, like an operator voiding
it. Operator credit notes moved off real time for the same reason one day
after this record was written (the 2026-07-29 credit-note fix that
[ADR-104](104-one-activity-lane-entity-calendar.md) records). Left on real
time, a failed payment on the hosted page sorts by its real time among the
invoice's simulated rows.

**Status.** Decided, not yet built. The change touches the settle path, so it
follows the [money-path playbook](../dev/money-path-robustness-playbook.md).
The dates PR of the ADR-128 series makes it; until it ships, the settle path
inserts these attempts with no test-clock time.

## Amendment 2026-10-20: Charge now is its own trigger, and attempts record their actor

**Change.** Writer 1 now records a third trigger. `charge_now` is a charge
someone asked for: "Charge now" in the dashboard, or
`POST /v1/invoices/{id}/collect`. `auto_charge` keeps the charges Velox
makes without that button: cycle close, finalize and the auto-charge
sweep. The PaymentIntent and its idempotency key are the same
for both, so Stripe sees no change. Writer 1 also stamps `actor_type` and
`actor_id` from `audit.ResolveActor`, the same values the audit log
records: whoever's request led to the charge. The trigger and the actor
are separate facts. `POST /v1/invoices/{id}/finalize` charges on the
request's identity, so its `auto_charge` attempt names the person or API
key that finalized; only charges no request started (cycle close, the
sweep, dunning) record `system`. Writer 2 records no actor: a settle report does not know who
paid. When writer 1's row follows a report's `external` row, it fills in
the trigger and the actor together.

**Why.** [ADR-128](128-activity-line-standard.md) §4 gives every activity
line an actor, and "why was this charged?" needs Charge now apart from the
automatic charges. Rows written before migration 189 keep `auto_charge`
and no actor; nothing is guessed for them.

**Rolling back.** The release before this one counts only `auto_charge`
and `dunning_retry` as Velox's own attempts when it judges a late payment
report (`veloxAttemptStandingTx`). It does not see a `charge_now` row, so
a late decline for an older automatic attempt can look like the newest
attempt. The invoice can then move back onto the older PaymentIntent, and
a second failure email can go out. During a rolling deploy this needs a
Charge now and a late decline for an earlier attempt within the minutes
both releases serve; that window is accepted. Rolling the binary back for
longer is not safe as is: first set those rows back with
`UPDATE invoice_charge_attempts SET trigger_source = 'auto_charge' WHERE trigger_source = 'charge_now'`,
the same statement migration 189's down step runs.
