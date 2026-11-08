# ADR-135: A restored database does not serve until it has caught up with Stripe

**Date:** 2026-11-09
**Status:** Accepted

## Summary

A database restored from a backup holds none of what Stripe did after the backup, so Hardcount could charge or refund that money again. Now a restore marks the database, and `hardcount serve` refuses to start while the mark is there. `hardcount restore catch-up` closes Hardcount's open payment pages at Stripe, then reads every Stripe event since the backup and applies it through the code that applies a webhook delivery. When nothing is left to do, it removes the mark. A payment whose invoice the restore lost, or left as a draft, cannot be applied: the catch-up lists it, and keeps the mark until the operator handles it by hand and acknowledges it. Billing stays paused after the catch-up, until the operator has redone the changes made after the backup.

## Context

A restore takes the database back to the moment of the backup. Stripe still holds the payments, refunds, disputes and saved cards since then. On the restored data:

- billing builds again an invoice the restore lost, and charges the card again;
- an invoice paid after the backup shows unpaid, so Collect payment, Charge now, a dunning retry or the hosted page can take a second payment;
- a refund made after the backup is missing, so the money shows refundable, and Hardcount can refund it again.

Until now, [Restore Hardcount](../ops/restore.md) had the operator hold every request path, pause five jobs, list Stripe's payments by hand, match each one to an invoice, and resend events. Three review rounds of that page each found a new way to charge twice. A person cannot reliably hold every path that charges a card, and the list of paths grows with the code.

Stripe documents the recovery for missed events: "If your webhook endpoint was temporarily unavailable, Stripe automatically retries undelivered events for up to three days. To speed up recovery, manually process the backlog. Call the List Events API with … `types`: The event types your endpoint listens for. … Stripe only returns events created in the last 30 days." ([Process undelivered events](https://docs.stripe.com/events/manage-webhook-endpoints.md#process-undelivered-events), read 2026-11-08.) After a restore, the events delivered to the old database are missing too, so the catch-up lists every event since the backup, delivered or not.

## Decision

1. **The restore marker.** Migration 00223 adds `restore_catchup`, a table of at most one row: when the database was marked, by what, and its anchor. The anchor is the newest instant the restored data shows Hardcount running: the newest leader-lease heartbeat or tick end, or the newest received Stripe webhook. The table has no tenant and no row-level security, like `leader_leases`. The limited app role may only read it, so no request can clear it.
2. **Marking.** `scripts/restore.sh` marks the database in the restore's own transaction, after the backup's data and privileges: it creates the table if the backup predates it, writes the marker with its anchor, and pauses billing (`leader_pause('billing', 'restore', …)`). A restore that rolls back leaves no marker and no pause. A marker the backup already holds is kept with its anchor. `hardcount restore begin` runs the same statements for a snapshot or point-in-time restore, and a test checks that the two texts agree.
3. **The gate.** `hardcount serve` reads the marker right after its schema check, and after `RUN_MIGRATIONS_ON_BOOT` migrations. With a marker it exits 2 before it listens or takes a lease. `migrate`, `migrate status`, `restore` and `version` are not gated.
4. **The catch-up** (`hardcount restore catch-up`, `internal/payment/restore_catchup.go`). It builds the services as serve does (`api.NewServer`), with no listener and no background jobs. It reads events created from 72 hours before the anchor, Stripe's redelivery window; `--since` can move that earlier, never later. It refuses a start more than 29 days ago, before calling Stripe. Then, for each tenant and mode with stored Stripe keys:
   1. It expires every open Checkout session of the hosted invoice page made for this tenant (`hardcount_purpose=hosted_invoice_pay`). This is its only write at Stripe. A session the customer pays while it closes is complete, and its payment is in the listing that follows.
   2. It lists the events of the twelve types the webhook handler processes, and applies them oldest first, events of the same second in id order. Each event goes through the webhook path's parse and tenant guards, then `Stripe.HandleWebhook`, unchanged. Its dedup row makes an event the database already holds, or a rerun, a no-op.
   3. A `payment_intent.succeeded` whose invoice is missing, or still a draft, is not applied. The catch-up lists it with its amount, customer and invoice id. The refund and dispute events of that payment are held with it: through `HandleWebhook` they fail on every try, because the payment names a Hardcount invoice that no row holds.
   4. Once the operator has handled such a payment (the manual steps in [Restore Hardcount](../ops/restore.md)) and passes `--acknowledge <pi>`, its event and held events get their dedup rows, with no other change.
5. **Exit codes.** 0: done, and the marker is deleted. 1: a temporary error; a rerun is safe. 2: refused before reading Stripe. 3: a listed payment is not acknowledged. 4: Stripe refused a live key. Only live-mode accounts decide the code; test-mode problems are reported and do not keep the marker.
6. **A key for this run.** `--stripe-key <tenant>:<live|test>=@<file>` replaces a stored key for this run only, and never saves it. It serves a key revoked since the backup, which the dashboard cannot fix while serve refuses to start, and an account connected after the backup, which the restored database has no key for.
7. **Row-level security.** Two new `TxBypass` uses, of a new kind `restore`: `markRestored` (the anchor reads `stripe_webhook_events` across tenants) and `tenantstripe.Store.ListAccounts` (the catch-up's list of accounts).

## Why this design

- **Not starting covers every path at once.** While the marker is there, nothing serves, so no request, job or page can act on the stale data, including paths added later. A runbook that lists paths to hold cannot promise that.
- **The catch-up adds no apply logic.** It feeds Stripe's own events to the code that applies a webhook, with that code's dedup. The live path and the catch-up cannot drift apart, and a rerun is safe.
- **The anchor comes from the restored data,** so the operator types no backup time. Lease ticks are written every few seconds on the database clock, so the anchor sits within seconds of the backup. The anchor is stored when the database is marked. The catch-up records the events it applies, so recomputing it on a rerun would move the start past events not yet applied.
- **Billing stays paused after the catch-up.** The changes made after the backup, such as a cancelled subscription, are still missing. Billing waits until the operator has redone them.

## Alternatives considered

- **A. Freeze the money paths while the server runs:** guard each charge, refund and hosted page with a restore flag. Rejected: once serve cannot start there is no request path to guard, and a freeze leaves other writes on stale data open, such as voiding an invoice that was paid after the backup.
- **B. Reconcile Stripe's objects instead of its events:** list PaymentIntents, refunds, disputes and sessions, and compare each with the database. Rejected: each object type needs its own adapter beside the live handler, which can drift from it. Its one gain, reaching back past 30 days, matters only for an old backup, which keeps a manual appendix.
- **C. Turn a payment whose invoice was lost into customer credit.** The credit would pay the invoice billing builds again, with no manual matching. Deferred, not rejected: it needs a credit grant keyed by PaymentIntent, and a way to treat the payment's later refunds. It waits for a restore that needs it. Until then the catch-up lists the payment.

## Consequences

### Positive

- After a restore, Hardcount cannot charge, refund or bill on data that lacks Stripe's history.
- [Restore Hardcount](../ops/restore.md) shrinks to a short procedure. Manual matching remains only for the payments the catch-up lists.

### Risks / open items

- **A backup older than 29 days** is refused: Stripe lists events for 30 days only. The restore page keeps manual steps for it.
- **Duplicate emails.** The catch-up's settles queue a receipt for a payment the old server may already have receipted. Outgoing emails and webhooks are queued, as in normal operation, and the restore page says how to skip the old ones.
- **Stripe Tax.** A tax transaction reported to Stripe Tax after the backup can be reported again for an invoice billing builds again. This is unchanged by this ADR.
- **A copy is not a restore.** Running the catch-up on a copy of the live database expires live customers' payment pages. The backup page says to remove the marker by hand on a copy instead.

### Settled while building

- **Payload fidelity.** List Events renders each event "according to Stripe API version at its creation time" ([List all events](https://docs.stripe.com/api/events/list.md)). A webhook endpoint uses its own version: "Stripe uses that version to structure the event payloads it sends to the endpoint" ([webhook versioning](https://docs.stripe.com/webhooks/versioning.md)). They differ only for an endpoint pinned to another version. The handlers read only long-stable fields: ids, `object`, `status`, `amount`, `currency`, `customer`, `metadata`, `created`, `payment_intent`, `charge`, `reason`, `failure_reason`, `payment_method`, `last_payment_error` and `evidence_details.due_by`. They read no field that a version renamed or removed, such as the PaymentIntent `charges` list. The catch-up keeps `data.object` as Stripe sent it, and a test checks that each of the twelve types parses to the same event as its webhook body.
- **PaymentIntents still in `requires_action` or `processing`.** Hardcount takes cards only. An off-session charge sets `error_on_requires_action`, so it never waits for the customer. A hosted-page payment that waits for the customer belongs to an open Checkout session, and Stripe says "You can't confirm or cancel the PaymentIntent for a Checkout Session. To cancel, expire the Checkout Session instead" ([Checkout Session object](https://docs.stripe.com/api/checkout/sessions/object.md)), which the catch-up does first. So no Hardcount payment can still be waiting when the events are listed, and the catch-up does not list PaymentIntents.
- **Accounts connected after the backup** have no key in the restored database. The operator passes the account's key with `--stripe-key`, and the catch-up reads that account too.

## References

- Migration 00223 (`restore_catchup`).
- `cmd/hardcount/restore.go`, `internal/payment/restore_catchup.go`, `scripts/restore.sh`.
- [ADR-114](114-leader-leases-tick-scoped-fencing.md): leader leases and `leader_pause`.
- [ADR-134](134-refunds-made-in-stripe-are-recorded.md): the refund handler the catch-up feeds.
- [ADR-127](127-a-lost-charge-result-is-resolved-by-asking-stripe.md): asking Stripe about a charge whose result is unknown.
- [Restore Hardcount](../ops/restore.md).
