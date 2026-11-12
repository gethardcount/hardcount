# ADR-040: Outbox is the only path (webhook + email)

**Status:** Accepted (amended 2026-08-30, 2026-11-09, 2026-11-10)
**Date:** 2026-05-30
**Supersedes:** the boot-time fallback contract introduced when
the webhook/email outboxes shipped (the `HARDCOUNT_WEBHOOK_OUTBOX_ENABLED`
and `HARDCOUNT_EMAIL_OUTBOX_ENABLED` env flags).

## Summary

Webhooks and emails always go through their outbox tables, and a background dispatcher delivers each row with retries and a dead-letter queue. The `HARDCOUNT_WEBHOOK_OUTBOX_ENABLED` and `HARDCOUNT_EMAIL_OUTBOX_ENABLED` flags were removed, because the direct-send path they enabled lost messages on any transient failure. Amended 2026-08-30 and 2026-11-09: every webhook event and the four money emails (payment receipt, payment failed, dunning warning, dunning escalation) are queued in their state change's transaction. Amended 2026-11-10: the team invite is queued in its invitation's transaction too. Operator, marker-re-driven and password-reset emails are queued after it. Only the drain, `Dispatcher.handle`, calls `dispatchFromOutbox`, which turns a queued webhook row into an event and its deliveries. To pause a dispatcher, run `SELECT leader_pause('email_outbox', who, why)` or the `webhook_outbox` equivalent ([runbook](../ops/runbook/background-job-stuck.md)); the advisory locks named below were retired by [ADR-114](114-leader-leases-tick-scoped-fencing.md). Status: active, amended.

## Context

Hardcount shipped transactional outboxes for outbound webhooks
(`webhook_outbox`) and customer-facing email (`email_outbox`) to make
delivery durable across crashes and transient downstream failures.
Producers persist an event/email intent inside the business-op
transaction; a background dispatcher drains the queue with retry +
DLQ.

> **Amendment (2026-08-30): the four money emails and all 41 webhook event types are queued in the transaction; other emails are queued after it.**
> The paragraph above is true of all 41 webhook event types: the five
> `invoice.*` events, the 16 `subscription.*` events, the nine `payment.*`
> events, the two `payment_method.*` events, the three `dunning.*` events,
> the two `customer.*` events, and the four `credit.*` events.
> `invoice.finalized`, `.voided`, `.marked_uncollectible` and
> `.payment_recorded` joined them on 2026-11-05: the invoice store queues
> each in the transaction of its change, for an invoice born finalized and
> a finalized draft alike. Twelve `subscription.*` events joined them on
> 2026-11-08. The subscription store queues each in the transaction of its
> change. That covers the operator's schedule, pause, resume and trial
> actions, the item changes, the threshold fire and the scheduled plan
> changes the billing run applies. `dunning.escalated` and `.resolved`, and
> `customer.email_bounced` and `.email_complained`, joined them on
> 2026-11-09. The dunning store queues `dunning.escalated` in the
> escalation's transaction, and `dunning.resolved` in the transaction of
> the resolve's compare-and-set, for its winner only. The customer store
> queues each suppression event with its write. The four payment anomalies
> (`payment.duplicate_charge`, `.amount_mismatch`, `.currency_mismatch` and
> `.received_on_voided_invoice`) and `payment_method.attached` and
> `.updated` joined them on 2026-11-09 too. The invoice store writes an
> anomaly's marker and queues its event in one transaction, once per kind
> and charge, and records `payment.amount_mismatch` in the card settle's
> transaction. The payment-methods store queues a card's event with the
> attach that changed the card. Before, these events went
> through `domain.EventDispatcher` → `webhook.OutboxDispatcher.Dispatch` →
> `OutboxStore.EnqueueStandalone`, a second transaction opened on the
> caller's context. A crash, a rolling deploy, a client disconnect, the
> 30-second route timeout or a lost leader lease between the two commits
> lost the event, and nothing sent it again. No producer uses that path
> now, and `internal/arch/webhook_emit_catalog_test.go` fails a new site
> that queues an event after its commit. It follows the event type through
> every wrapper that passes it on, and through the interfaces those
> wrappers implement, such as the payment adapter's anomaly recorder.
> (Narrowed 2026-11-09: the amendment below says what the test checks and
> what it misses.)
> (Corrected 2026-11-05. This paragraph first said that every webhook
> producer enqueues inside the business transaction.)
> The paragraph above has never been
> true of `email_outbox`: all ten `OutboxSender` sites enqueue via
> `OutboxStore.EnqueueStandalone`, which opens its own transaction
> AFTER the business transaction commits; the in-tx primitive
> `Enqueue(ctx, tx, …)` has no production caller besides
> `EnqueueStandalone` itself. The email outbox is therefore a durable
> at-least-once delivery queue *from the moment its row commits*, and
> the commit→enqueue gap (a process death or database failover between
> the two commits) loses the email with the state standing — the
> exactly-once gates on the money path then suppress every later
> attempt. Under the N ≥ 2 production posture that gap is hit by routine
> failovers, so the money emails move into the state-transition
> transaction. **Shipped 2026-08-31, both halves.** Dunning pair
> (warning + escalation): `UpdateRunIfActive` takes a `then` hook that
> runs the outbox enqueue inside the state transaction, under a
> SAVEPOINT — an email-side failure logs loud and skips the email, never
> the money write. Settle pair (payment receipt + payment failed): the
> same shape on `MarkPaidCardSettlementTransition` and
> `MarkPaymentFailedReportingTransition`, gated on the transition each
> already reports, so a duplicate settle re-fires nothing; the hook is
> handed the freshly-written row because the receipt's amount is what
> that transaction booked. Because the settle tx now carries a
> customer-visible effect, the cancel-detach (`context.WithoutCancel`)
> moved AHEAD of it — a webhook client hanging up mid-settle must not
> abort a transaction that owes the customer a receipt. Account-plane and
> operator-initiated sends stay post-commit (the operator receives the
> enqueue error synchronously).

> **Amendment (2026-11-09): every webhook event is queued in the transaction of the change it reports.**
> An outbox row is a database write, not an external effect; only its
> delivery is asynchronous. A producer inserts it with
> `OutboxStore.Enqueue(ctx, tx, …)` in the transaction that writes the
> state change, usually in the store method that owns that transition (the
> MarkPaid pattern). It builds the payload from the row that transaction
> wrote (ADR-131). The event commits with its change, and an enqueue
> failure fails the change. All 41 webhook event types follow this rule
> (P1-01), so a crash after a commit can no longer drop an event.
> `invoice.finalized` has one writer, the invoice store, for an invoice
> born finalized and a finalized draft alike.
>
> P1-01 removed the post-commit path: `domain.EventDispatcher`,
> `webhook.OutboxDispatcher`, the webhook `OutboxStore.EnqueueStandalone`,
> `webhook.Service.Dispatch` and every service's `SetEventDispatcher`. It
> also removed the `hardcount_webhook_outbox_enqueue_failures_total`
> metric, which counted the events that path lost.
> `internal/arch/webhook_emit_catalog_test.go` fails if the webhook package
> gains a function or method that takes an event type but no `*sql.Tx`. It
> does not see a hand-rolled `BeginTx` + `Enqueue` + `Commit` with no state
> write in it; the class B question of the
> [money-path playbook](../dev/money-path-robustness-playbook.md#b-half-saved)
> covers that. `webhook.MustWiredOutbox` refuses to boot when a store that
> queues events has no outbox, because such a store skips its events.
>
> `EnqueueStandalone` remains for email only, where no business
> transaction exists:
> - an operator's send or resend (invoice, credit note, setup link), where
>   the request returns the enqueue error;
> - an engine email that its own marker column sends again until the send
>   is recorded: the collector's invoice email (`invoice_emailed_at`) and
>   the no-payment-method notice (`no_pm_notified_at`);
> - an account-plane send: a password reset, which the requester asks for
>   again. (Narrowed 2026-11-10: a team invite is now queued on its
>   invitation's transaction, and a failed queue writes no invitation. See
>   the [ADR-081 amendment](081-minimal-team-invites.md#amendment-2026-11-10-each-membership-change-is-one-transaction).)
>
> `internal/arch/email_outbox_standalone_test.go` lists them with their
> reasons, and CI fails on any other call.

At migration time, both outboxes shipped behind boot env flags
(`HARDCOUNT_WEBHOOK_OUTBOX_ENABLED=true` and `HARDCOUNT_EMAIL_OUTBOX_ENABLED
=true`) so operators could roll back to the legacy direct-dispatch
path on regression. The router used the flag to switch BOTH the
producer wiring (`OutboxSender` ↔ `*email.Sender`, `OutboxDispatcher`
↔ direct `Service.Dispatch`) AND the dispatcher worker startup
(skipped when the flag was off, since nothing would be enqueued).

Three observations on 2026-05-30 forced a re-evaluation:

1. **The flag couples producer and consumer.** There's no way to
   independently disable the dispatcher (e.g. for the FLOW E7 test
   that wants to observe a pending row sit in `email_outbox` while
   the dispatcher is paused). The same env var moves both ends
   together.
2. **The "off" path silently weakens correctness.** With the flag
   off, the same Go call site (`SendPaymentReceipt`, `Dispatch`,
   etc.) becomes a one-shot synchronous call. Transient SMTP / HTTP
   5xx failures bubble out as errors that the caller logs and
   discards — the email or webhook is lost forever. The flag
   trades durability for nothing in return, and the trade is
   invisible at the call site.
3. **Boot-time flags are the wrong tool for the operational concerns
   they're meant to address.** Multi-replica safety is already
   handled by the dispatcher's advisory-lock gating
   (`LockKeyWebhookDispatcher` / `LockKeyEmailDispatcher` —
   `internal/platform/postgres/advisory_lock.go`). Runtime pause
   ("stop sending email RIGHT NOW") wants a runtime API or external
   advisory-lock hold, not a restart-required env flag. Worker /
   API binary split is a deployment topology decision (`cmd/hardcount-worker`
   if it ever ships), not a feature flag.

The flag also violates two project principles:

- **`feedback_no_silent_fallbacks`** — engine paths that can't
  produce a correct answer must fail loudly, not substitute a
  default. The direct-dispatch / direct-SMTP path on a transient
  failure silently drops the side-effect.
- **`feedback_no_belt_and_suspenders`** — one path unless
  independent failure modes warrant two. Outbox failure modes (DB
  unreachable, dispatcher crashed) are NOT independent of direct-
  send failure modes (SMTP unreachable, sender crashed). One can't
  rescue the other.

## Decision

**Cut both flags. The outbox is the only path.**

- `HARDCOUNT_WEBHOOK_OUTBOX_ENABLED` removed. Producers always enqueue
  to `webhook_outbox`; the dispatcher worker always starts.
- `HARDCOUNT_EMAIL_OUTBOX_ENABLED` removed. Producers always enqueue
  to `email_outbox` via `OutboxSender`; the dispatcher worker
  always starts.
- `Server.OutboxEnabled` and `Server.EmailOutboxEnabled` fields
  removed from `internal/api/router.go`.
- Boot-time WARN logs ("outbox DISABLED — using legacy direct-
  dispatch path") removed — there's no longer a DISABLED state.
  The boot-time INFO logs ("outbox enabled — producers will
  enqueue…") also removed; the outbox being on is now an
  invariant, not a state worth logging.
- `.env.example` flag entries removed.
- The direct-SMTP `*email.Sender` stays — but only the dispatcher
  worker calls it now (its real purpose: convert an outbox row
  into an actual SMTP send). Same for `webhook.Service.Dispatch`:
  only the dispatcher calls it now.

## Runtime concerns the flag was meant to address

| Concern | What the flag tried | New answer |
|---|---|---|
| Multi-replica: only one drains | Disable on N−1 replicas | Advisory lock already handles this; first claimant wins, all others sleep |
| Emergency pause | Set flag false + restart | Hold the advisory lock from an external psql session (no restart) |
| Separate worker process | Skip the goroutine on API replicas | When/if this is needed: split into `cmd/hardcount` (API) + `cmd/hardcount-worker` (dispatcher-only); topology in deploy, not code |
| Local dev quiet | Set flag false | No traffic = no rows = quiet dispatcher (it polls every 5s, claims nothing) |

All four are dynamic OR deployment-shape concerns; none of them
need a boot env flag.

## Consequences

- **Operators lose no real capability.** The flag was never set to
  `false` in any deployment; `.env.example` shipped with both at
  `true`. There's no DP and no production user to break.
- **Test surface area halves.** Every email / webhook producer
  change had to be considered against both paths; now there's one.
- **Boot complexity drops.** Router wiring loses two env-parse
  blocks + their dual-branch if/else; the seven typed interface
  variables collapse from `var X type; if … { X = a } else { X = b }`
  shape to one-line declarations.
- **FLOW E7 becomes runnable cleanly.** The test was effectively
  blocked on the absence of a separate dispatcher-disable flag.
  The advisory-lock pause technique works today and is documented
  in the updated FLOW.
- **Future operator-pause API.** When a DP names "I want to pause
  email delivery for tenant X mid-flight," the right place to add
  that is a runtime API (per-tenant flag in `tenant_settings`,
  honored by the dispatcher when claiming rows) — not a return
  of the boot flag.
- **Migrations / data unchanged.** `webhook_outbox` and
  `email_outbox` schemas, their dispatchers, DLQ tables, advisory-
  lock keys all stay as-is.

## What stays available for legitimate use

- `pg_advisory_lock(76540004)` from any psql session: pauses the
  email dispatcher cluster-wide until released.
- `pg_advisory_lock(76540003)` (or whatever the webhook lock key is):
  same for webhook dispatcher.
- These ARE the runtime pause primitives. They were already there;
  the flag was never the right interface.

## Revisit trigger

Resurrect the flag (or a runtime equivalent) when:
- A DP names a use case where producer enqueue must be skipped
  cluster-wide (e.g. compliance regime requires "no email storage
  even transient"), OR
- A binary split (`cmd/hardcount-worker`) is shipped and one side
  needs to know which role it's running.

Until then, "outbox on" is an invariant.

## Industry references

- Stripe, Lago, Chargebee, Recurly: none expose a boot-time flag
  that switches email/webhook delivery from outbox-backed to
  direct. Pause is a runtime per-endpoint API.
- Sidekiq, Hangfire, Resque (background-job ecosystems): split
  binary (worker mode vs web mode), not env-flag. Always-on within
  the role.
- PostgreSQL `pg_advisory_lock` is the canonical primitive for
  external runtime pause of a database-backed worker; it's what
  the dispatcher already uses for multi-replica gating.
