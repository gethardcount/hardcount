# ADR-060: Card-less invoices enter dunning (no-payment-method enrollment)

## Status

Accepted — 2026-06-23 (amended 2026-10-08 and 2026-10-30).

## Summary

An unpaid invoice whose customer has no usable card enters dunning (the retry-and-escalate process), like a declined charge. Without this, the subscription stayed active and kept billing with no path to a final decision. The retries count as failed attempts, so the run ends in the policy's final actions, and adding a card mid-run lets the collector charge the invoice. Amended 2026-10-08: the auto-charge collector starts the run itself when money is still owed and no card exists ([ADR-116](116-collection-intent-at-finalize-one-collector.md)), and the separate enrollment sweeps were deleted. Amended 2026-10-30: without Stripe for the invoice's mode no card can be added, so the customer is emailed the invoice itself once, not a card-setup link. The run's retries then charge nothing and remind the customer to pay the invoice, and the run still ends in the policy's final actions. Status: active, amended; tenants with dunning disabled are skipped.

## Context

A product-wide **liveness / termination-invariant** audit (every stateful
entity must reach a terminal state; all entry points into a shared state
must converge) surfaced an absorbing sink in the collection path.

When a subscription invoice finalizes for a customer with **no resolvable
payment method**, the engine flags it `auto_charge_pending=true`, sends one
"add a payment method" email, and moves on. The background auto-charge
sweep (`RetryPendingCharges`) re-checks it every tick — but with no card
there is nothing to charge, so it does nothing, indefinitely.

Crucially, dunning — the machine that escalates a failing invoice to a
**terminal** (`pause` / `cancel_subscription` / `mark_uncollectible`) — only
starts from an *actually-attempted-and-declined* charge (`ChargeInvoice`
fires `StartDunning` inline on a decline). A no-card invoice never attempts
a charge, so it never enters dunning, so it never reaches a terminal:

- the subscription stays `active`,
- it keeps drafting invoices each cycle,
- the unpaid balance grows **unbounded**,
- nothing ever forces a pause/cancel/write-off decision.

This is the asymmetry: the **declined-card** entry point escalates; the
**no-card** entry point silently dead-ends. It is also a `feedback`-class
silent fallback — a path that cannot produce a correct outcome parks
instead of failing loud.

Industry framing (verified): keeping an unpaid subscription *active during
recovery* is the norm (Stripe/Chargebee/Recurly/Orb/Lago/Metronome); the
defect is not "active while unpaid" but "active while unpaid **with no
off-ramp**." Grace is correct only if it terminates.

## Decision

Route card-less `auto_charge_pending` invoices into the **same dunning
machinery** the declined-card path already uses, rather than inventing a
parallel recovery system or a new subscription state.

Mechanism:

- The engine gains an optional `DunningStarter` interface (mirroring
  `NoPaymentMethodNotifier`), wired in `router.go` to a thin adapter over
  `dunning.Service`. Nil = inert (local dev / tests).
- *Amended 2026-10-08 ([ADR-116](116-collection-intent-at-finalize-one-collector.md)):*
  the auto-charge collector starts the run itself, in its no-card arm: after
  the credit step succeeded, with money still owed and no chargeable payment
  method (`ResolveForCharge` found none; a lookup error starts nothing). It is
  the no-card twin of the charger starting dunning on a decline. The separate
  `EnrollStalledForDunning` / `…ForClock` sweeps that read the queue are
  deleted: once every unpaid invoice was queued from its finalize write, the
  queue alone no longer meant "card-less", and a reader of it could dun an
  invoice whose credits were about to cover it. `StartDunning` is
  one-run-per-invoice, so a revisit of an invoice that already has a run is a
  no-op.
- *Original mechanism (2026-06-23, superseded above):* `EnrollStalledForDunning`
  (cron) and `EnrollStalledForDunningForClock` (catchup) listed the
  `auto_charge_pending` candidates after `RetryPendingCharges` and called
  `StartDunning` for each.
- The dunning retrier (`RetryPayment`) already returns a *real failed
  attempt* on "no payment method" (not `ErrTransientSkip`), so the campaign
  ticks through grace + retries and **exhausts to the policy
  terminal actions** (ADR-112 split these into `final_subscription_action`
  and `final_invoice_action`) — the card-less delinquent reaches a terminal.
- Adding a card mid-campaign lets the existing auto-charge sweep collect it
  and the run resolves `payment_recovered`.
- A tenant with dunning **disabled** keeps the invoice un-dunned: the
  adapter swallows the `dunning is disabled` `InvalidState` as a deliberate
  no-op (same outcome the declined-card path gets with dunning off) rather
  than emitting a per-invoice error every tick.

No migration: reuses the `auto_charge_pending` flag, the candidate queries,
and the dunning state machine.

## Consequences

- The no-card limbo sink is closed on both the wall-clock and test-clock
  paths; a card-less subscription now reaches a terminal exactly as a
  declined-card one does.
- Telemetry: a failed start is returned as a collector error (logged by the
  sweep, surfaced by catchup) and retried on the next tick, because the
  invoice stays queued.
- Minor wart: a no-card dunning run ticks `attempt_count` on retries that
  never attempt a charge ("attempt N of M" on a never-charged invoice). The
  dunning-list label keys on the run so it reads as a no-payment campaign,
  not a card decline. A two-phase accounting refinement (don't tick until a
  card exists) is a deferred follow-up.

## Alternatives considered

- **An explicit `incomplete`/gated subscription state (Stripe
  `payment_behavior`).** Rejected as the *default* — Hardcount is B2B,
  operator-gated, PaymentIntent-only; create-live matches the AI-native peer
  set (Orb/Lago/Metronome). An *opt-in* gate is deferred (below).
- **A persisted `collection_state`/`past_due` subscription column.**
  Rejected — it would be a second source of truth that desyncs from invoice
  truth (verified: `SettleSucceeded` doesn't resolve dunning runs, so a
  stored status would lie on self-pay/reconciler paths). Health, when
  surfaced, will be *derived* on read.
- **Do nothing (pre-launch, no victim yet).** Rejected — this is a silent
  money/recovery sink in a core path, the class Hardcount fixes at the root
  regardless of stage; the fix is contained and zero-migration.

## Deferred (trigger-gated, for a design partner)

- Operator `collection_health` signal (`current|past_due|unpaid`), derived
  on read.
- Opt-in first-payment gate (`require_payment`, reusing `draft`).
- An `unpaid` access-revoke terminal between `pause` and `cancel`
  (Stripe's revoke-but-recoverable step).
- Usage spend-cap hard-stop / prepaid-balance (the AI-native exposure
  limiter for unbounded unpaid usage).

## Amendment (2026-10-30): without Stripe, the invoice email instead of a setup link

**Problem.** The no-card arm assumed a card could be added. When the tenant
has no Stripe account connected for the invoice's mode (livemode), it cannot:

- the setup-link email led to a form that could not save a card;
- every dunning reminder asked for a card the customer could not add;
- the invoice itself was never emailed: only an operator's **Email invoice**
  sends it.

**Decision.** Without Stripe for the invoice's mode, the tenant collects like
a send-invoice tenant (Stripe Billing's `collection_method=send_invoice`;
Lago and Orb send the invoice too):

- **First contact is the invoice email.** The collector emails the customer
  the invoice once: the PDF and the hosted invoice page link, the same email
  as **Email invoice**, to the customer's address and stored CC list. The
  send-once marker is a new column, `invoices.invoice_emailed_at` (migration
  220); `no_pm_notified_at` keeps meaning "a setup link was emailed". No setup
  link is sent. The hosted page says online payment isn't set up and to
  contact the seller.
- **The run still starts** (cause `no_payment_method`), so the invoice still
  reaches the policy's final actions. The liveness reason above holds: without
  the run, an unpaid invoice with no card would again have no off-ramp.
- **Retries charge nothing and are reminders.** The retrier checks the
  connection before it reads the card: without Stripe it charges nothing,
  even with a card on file from before, and reports
  `ErrNoOnlinePaymentOnRetry`. That is a kind of `ErrNoPaymentMethodOnRetry`,
  so the attempt counts and the timeline shows a retry that charged nothing.
  The reminder (`decline_ask` `pay_invoice`) asks the customer to pay the
  invoice, links the hosted page as **View invoice**, and says nothing about
  a card or a retry date.
- **Connecting Stripe later** puts the invoice back on the path above: the
  next visit sends the setup link once, and the next retry reads the card.
- **A failed read of the connection is not "not connected".** The collector
  counts an error and sends and starts nothing; the retrier skips the tick
  uncounted. The invoice stays queued, and the next visit reads it again.
- **The invoice email follows the operator's rules.** It is refused for a
  draft, a void or a payment in flight, and without SMTP (`SMTP_HOST` unset):
  nothing is stamped, a `WARN` is logged, and the next visit tries again. A
  customer with no email address is skipped the same way, as for the setup
  link.
- **The invoice's notice** says Stripe isn't connected in this mode, so no
  card can be added online, and says what happened to the invoice email. It
  offers no **Resend setup link**.

"Connected" is the hosted page's own check (`payment.StripeClients.HasFor`
for the invoice's mode), given to the billing engine and the dunning retrier
as narrow interfaces. The email is the invoice handler's **Email invoice**
body, shared by both senders.

**A run started while Stripe was connected**, then Stripe disconnected, is
left to carry on: its next retry finds no Stripe, charges nothing, and sends
the pay-the-invoice reminder. The operator sees the run on the Dunning page.

**A card saved before Stripe was disconnected** is not charged either. The
collector reads the connection before it charges, so such an invoice takes
the same no-Stripe path: the invoice email once and the dunning run. Before,
each visit tried the charge, got "Stripe not configured", and skipped it, with
no email and no run.

**Save & send** reads `invoice_emailed_at` on the finalize response (the API
returns it). When finalizing already emailed the invoice, the dashboard does
not send it again, so the customer gets it once.

**Not settled here.** The final notice (dunning escalation) keeps its **Pay
invoice** button, which opens the hosted page that says to contact the seller.
