# ADR-013: Invoice Attention — Honest Collection-State Surface

**Date:** 2026-05-01
**Status:** Accepted

## Status
Accepted

## Date
2026-05-01

Refines ADR-009 (Invoice Attention) by tightening the semantics of
the time fields and adding a new attention reason for the operator-
actionable "no payment method" state.

## Summary

A finalized, unpaid invoice whose customer has no payment method gets its own attention reason, `no_payment_method`, separate from `awaiting_payment`, so the operator learns why nothing is charged. `next_attempt_at` is set only when the engine has a real retry scheduled, and the payment deadline is a separate `due_by` field. The customer is emailed once, and adding a card later charges the invoice unless collection is paused. Since then, card-less invoices also enter dunning ([ADR-060](060-no-payment-method-dunning-enrollment.md)), and the `invoice.due_by` timeline row was removed because a future deadline is not an event. Status: active.

## Context

Three operator-confusion bugs surfaced during the Velox v1 dashboard
review, all rooted in the same shape: the invoice-detail surface
was emitting *plausible-sounding* signals that didn't match what the
billing engine actually does.

### Bug 1: "Awaiting payment" hides the WHY

A finalized invoice with no PaymentSetup attached classifies as
`awaiting_payment` with the message `"Invoice is finalized and
awaiting payment. No charge attempt has fired yet."`

The engine's actual behaviour (`internal/billing/engine.go:1364`):

```go
if e.charger != nil && e.paymentSetups != nil && inv.AmountDueCents > 0 && !collectionPaused {
    if ps, err := e.paymentSetups.GetPaymentSetup(...); err == nil &&
        ps.SetupStatus == domain.PaymentSetupReady && ps.StripeCustomerID != "" {
        // ChargeInvoice synchronously
    }
    // else: silently skip. No retry. Nothing scheduled.
}
```

When no PM is attached, the engine doesn't auto-charge, doesn't queue
a retry, and doesn't fire dunning. The invoice sits at
`payment_status=pending` forever until either (a) the operator adds a
PM and Charges now, (b) the customer self-pays via the hosted invoice
link, or (c) the invoice becomes overdue (then dunning policy kicks
in independently).

The original `awaiting_payment` framing made this look like a
transient, system-managed state. It isn't — the operator is the only
mechanism. The "WHY" the operator needed wasn't surfaced.

### Bug 2: `next_attempt_at = due_at` is a category error

ADR-009's earlier banner work added a `NextAttemptAt` field to the
attention payload and populated it from `inv.DueAt` for the
`awaiting_payment` and `payment_scheduled` reasons. This was wrong by
construction:

- **Auto-charge fires at finalize**, not at due_at. With a PM
  attached, the charge already happened (success → invoice paid;
  failure → `auto_charge_pending=true`).
- **Due_at is a deadline**, not a scheduled engine action. It marks
  when the invoice becomes overdue → dunning policy considers
  retries (separate codepath from `auto_charge_pending`).

Surfacing `next_attempt_at = due_at` told operators the engine would
auto-act on a date when it has no such intent. For no-PM invoices,
the engine has no scheduled action at all; for has-PM invoices, the
auto-charge has already concluded.

### Bug 3: `invoice.auto_charge_scheduled` lifecycle timeline event

Same root cause: the timeline injected a synthetic
`Auto-charge scheduled at <due_at>` event for every finalized
unpaid invoice. The event is misleading in every case:

- No PM: there is no scheduled auto-charge. Ever.
- Has PM, charge succeeded at finalize: the actual webhook event
  supersedes this row anyway.
- Has PM, charge failed → `auto_charge_pending`: the scheduler retries
  on its sweep tick (seconds-to-minutes), nowhere near due_at.
- Has PM, dunning retrying after the first failure: the next retry
  is from the dunning policy schedule, not due_at.

## Decision

### Add a new attention reason: `no_payment_method`

Distinct from `awaiting_payment`. Branches the attention classifier
on a new `AttentionContext{HasPaymentMethod bool}` parameter:

```go
case inv.Status == InvoiceFinalized && inv.PaymentStatus == PaymentPending && !atc.HasPaymentMethod:
    return classifyNoPaymentMethod(inv)
case inv.Status == InvoiceFinalized && inv.PaymentStatus == PaymentPending:
    return classifyAwaitingPayment(inv)
```

Reason payload:

- **Severity**: `warning` (operator action required, not financially
  broken — promoting to `critical` would make a perfectly normal
  send-invoice collection mode look alarming).
- **Code**: `payment.no_payment_method`.
- **Message**: "No payment method on file. The engine won't
  auto-charge until one is attached. Add a payment method, send the
  invoice link for self-pay, or void the invoice."
- **Actions**: `add_payment_method` (primary, deep-links to
  `/customers/:id`) + `send_reminder` (secondary, send the hosted
  invoice link).

`AttentionContext` is the seam for future signals (dunning policy
retry schedule, collection mode, etc) without bloating the Invoice
domain struct. Zero-value is safe — falls through to the existing
`awaiting_payment` branch when callers don't have the signal.

Stripe parity: equivalent to their
`customer.invoice.requires_payment_method` flow, scoped to a single
attention reason.

### Tighten the time-field semantics

Three distinct time fields, one purpose each:

| Field | Meaning | When to populate |
|---|---|---|
| `Since` | When the state began | Always |
| `NextAttemptAt` | When the engine **will retry automatically** | Only when there's a real scheduled retry: `payment_scheduled` (auto_charge_pending sweep), dunning retry. Never for awaiting_payment / no_payment_method. |
| `DueBy` | Payment deadline (mirrors `invoice.due_at`) | When the invoice has a `due_at` set and is operator-relevant |

Removed `NextAttemptAt = inv.DueAt` from `classifyAwaitingPayment` and
`classifyPaymentScheduled`. Added `DueBy` (also `inv.DueAt`) to both.
Banner renders the two fields under separate copy: "Engine will
retry: …" vs "Due by: …".

### Replace the bogus timeline event

`invoice.auto_charge_scheduled` lifecycle event removed.

Replaced with `invoice.due_by` — a passive deadline marker, only
emitted when the invoice is finalized + pending + has `due_at`. The
description is "Payment deadline", which is accurate (it's the
deadline, not a scheduled action).

The actual auto-charge event flows from Stripe webhooks
(`payment_intent.succeeded` / `payment_intent.payment_failed`) into
the timeline via the existing `relevantStripeEvents` path — no
synthetic event needed.

## Implementation

- **Backend**: `domain.AttentionContext`, `Attention.NextAttemptAt`
  (re-scoped), `Attention.DueBy` (new). `classifyNoPaymentMethod`
  added; `classifyAwaitingPayment` and `classifyPaymentScheduled`
  drop `NextAttemptAt = DueAt` and add `DueBy`.
  `invoice.Service.attachAttention` becomes a method, reads
  `PaymentMethodReader`, populates the context. Router wires
  `customerStore` (which already implements `GetPaymentSetup`).
- **Timeline**: `invoice.auto_charge_scheduled` event removed;
  `invoice.due_by` (passive) added with description "Payment
  deadline".
- **Frontend**: `humanReason` and `defaultLabel` learn the new
  `no_payment_method` reason and `add_payment_method` action.
  Banner renders `NextAttemptAt` and `DueBy` with distinct copy.
  `add_payment_method` action deep-links to the customer page (same
  as `edit_billing_profile`).
- **Tests**: `TestClassifyInvoiceAttention_NoPaymentMethod` pins the
  new reason. `TestClassifyInvoiceAttention_AwaitingPayment` updated
  to opt into `HasPaymentMethod=true` and assert `NextAttemptAt` is
  null (no scheduled engine action).

## Consequences

### Migration

Velox is pre-launch (single tenant, no design partners). No backfill
needed. Existing finalized-pending invoices on the dashboard
re-classify on the next read.

### `attention.NextAttemptAt` semantics changed

Pre-this-change, `NextAttemptAt` was overloaded with `due_at` for
awaiting/scheduled. Any consumer that read it to predict "when will
the engine act" was wrong half the time. Post-this-change,
`NextAttemptAt` is null for awaiting/no-PM (correct) and reflects
real scheduled retries elsewhere. Consumers that mistakenly relied on
the old shape should switch to `DueBy` for deadline display.

### Engine queues for retry, fires customer notification — production-grade

Initial implementation silently skipped auto-charge when no PM was
ready. This was acceptable for local-only single-operator usage but
unacceptable for paying customers in production: the customer is
never notified, the operator must manually monitor open invoices,
and attaching a PM later does nothing automatic. Refined to match
industry standard:

1. **Engine queues for scheduler retry**: at finalize, when no PM is
   ready, engine sets `auto_charge_pending=true` instead of silent
   skip. The existing `RetryPendingCharges` scheduler path checks PM
   each tick — skips while still missing, charges immediately when
   the customer attaches one. This is Chargebee's "Collect Invoice
   on Card Update" — the operator never has to babysit, the
   customer's attach-PM action self-resolves the open invoice.
2. **Customer notification at finalize**: engine fires
   `NoPaymentMethodNotifier` (wired in router.go to the existing
   `payment.EmailPaymentUpdate` path that Stripe already uses on
   charge failures). Customer receives the same "Action required:
   payment method needed" email format with a tokenized
   payment-update URL — at finalize, not weeks later when the
   invoice goes overdue.
3. **Classifier priority**: `no_payment_method` now beats
   `payment_scheduled`. When both flags are set (engine queued for
   retry but PM still missing), the actionable reason wins —
   `payment_scheduled`'s "engine will retry on its next tick"
   message would lie to the operator (the retry would skip again
   until a PM is attached).

Both engine wiring and notifier dispatch are optional (nil-safe) so
narrow unit tests don't have to wire the full email infrastructure.

### Two-step framing for `no_payment_method`

Even with auto-fire-on-PM-attach in place, the in-page actions still
name the manual override path explicitly. The message:

> "No payment method on file. To charge, attach a method then click
> Collect Payment. To let the customer pay themselves, share the
> invoice link."

The Collect Payment button on the invoice detail page is **disabled
with a tooltip** ("Attach a payment method first") when the customer
has no PaymentSetup ready. Same disable-with-tooltip pattern Velox
uses for Finalize-on-tax-failure. The InvoiceAttention banner above
provides the path forward so the disabled button isn't a dead-end —
it's a visible constraint with a clear link to the fix.

### Future enhancements (deferred)

- `NextAttemptAt` for `payment_failed` derived from
  `dunning_runs.next_action_at` (needs a join, not in this ADR's
  scope).
- Engine-side `next_sweep_at` so `payment_scheduled` can also surface
  a precise retry time. Today the cadence is short enough that "on
  its next tick" is honest.
- Collection-mode field on subscriptions (`charge_automatically` /
  `send_invoice`) so `no_payment_method` framing can adapt to the
  customer's intended collection method (today the message
  accommodates both implicitly).

## Industry references

- **Stripe**: `customer.invoice.requires_payment_method` (no-PM),
  separate `next_payment_attempt` field for scheduled retries
  (distinct from `due_date`).
- **Chargebee**: `payment_method_status` on subscription, distinct
  attention shapes for "missing PM" vs "retry pending".
- **Lago**: deadline (`due_date`) and engine-action (`next_attempt`)
  are separate fields with distinct semantics.

Velox now matches this convention.
