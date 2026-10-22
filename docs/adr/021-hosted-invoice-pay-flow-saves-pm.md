# ADR-021: Hosted-invoice Pay flow saves the card to the customer's Stripe customer

**Status:** Accepted (amended 2026-10-11, 2026-10-18 and 2026-10-22)
**Date:** 2026-05-03

## Summary

Paying on the hosted invoice page saves the card for later off-session charges. The Stripe Checkout session names the customer's Stripe customer (created on first use) and sets `setup_future_usage=off_session`, so future invoices can charge the card without the customer present. Amended 2026-10-11: Velox also records that card in its own `payment_methods`, through `Stripe.RecordCheckoutCard`, from the webhook or the reconciler's hosted sweep. The card becomes the default only when the customer had no card on file. Amended 2026-10-18 (ADR-126): it also replaces a default the bank refused for good. Amended 2026-10-22: a card saved through a Velox setup or update link always becomes the default; a card paid on the hosted page keeps the rule above. Status: active, amended.

## Context

The hosted invoice page (Stripe-equivalent `hosted_invoice_url`) has a
Pay button that opens a Stripe Checkout session for the outstanding
balance. The end customer can tick "save card for future payments"
during that Checkout flow.

Operator-reported bug: customer paid an invoice via the hosted page,
ticked the save-card box, the payment succeeded — but the customer's
detail page in Velox kept showing "no payment method on file" and
later invoices fell into dunning instead of auto-charging.

Root cause was in `internal/api/adapters.go::CreateInvoicePaymentSession`.
The Checkout session was created in `mode=payment` with:

- **no `Customer`** — guest checkout, no Stripe customer object to
  attach the PM to.
- **no `setup_future_usage`** — the PM is consumed by the one-shot
  PaymentIntent and detached.

Even though the customer ticked "save," Stripe had no customer record
to attach the PM to and no instruction to keep it. The PM existed
transiently for the charge and then evaporated. `handleCheckoutCompleted`
correctly skipped the `customer_payment_setups` upsert (the
[ADR-020 follow-up fix from 2026-05-04](./020-invoice-timeline-coalesce-and-card-detail.md)
makes the upsert conditional on `FetchCardDetails` finding a PM
attached to the Stripe customer — which there was none to find).

The `/update-payment` flow from a no-PM email already does this
correctly — `mode=setup` with `Customer: stripeCustomerID`. The Pay
flow was the outlier.

## Decision

Pay-flow Checkout sessions now pass:

1. `Customer: stripeCustomerID` — resolved (or lazily minted) via
   `paymentmethods.StripeAdapter.EnsureStripeCustomer`. This is the
   same helper the customer portal already uses, so the portal-created
   customer and the Pay-flow customer share the same Stripe customer
   record.
2. `PaymentIntentData.SetupFutureUsage: "off_session"` — the canonical
   Stripe pattern from
   [Save card during payment](https://docs.stripe.com/payments/save-during-payment).
   The `off_session` value (vs. `on_session`) means Velox can charge
   this card later without the customer present, which is what dunning
   auto-retry needs.

`hostedInvoiceStripeAdapter` gains a one-method `stripeCustomerEnsurer`
interface dependency rather than importing `paymentmethods` directly,
keeping `internal/api/adapters.go` from depending on a peer package
beyond the wiring layer. `paymentmethods.StripeAdapter` satisfies it.

Wiring order in `router.go` shifted: `paymentMethodsStripe` is now
constructed earlier (next to the hosted-invoice adapter wiring) so it
can be passed into both the hosted-invoice adapter AND the
customer-portal payment-methods service later.

## Consequences

**Save-card on Pay actually saves the card.** When the end customer
ticks the box (or with `off_session`, it's saved unconditionally —
Stripe shows "Cardholder authorizes future charges" disclosure copy
instead of an opt-in checkbox), the PM is attached to the Stripe
customer and:

- `checkout.session.completed` webhook → `handleCheckoutCompleted`
  finds the attached PM via `FetchCardDetails(stripeCustomerID)`,
  upserts `customer_payment_setups` with `setup_status='ready'` +
  card brand + last 4.
- `payment_intent.succeeded` webhook → invoice marked paid + card
  details stamped on the invoice via `SetPaymentCard` (ADR-020).
- Future invoices auto-charge via the existing dunning path (no
  email/Update-Payment-Method round-trip needed).

**A Stripe customer is minted on first hosted-invoice payment** for
any Velox customer that doesn't already have one. The current
`EnsureStripeCustomer` mints a bare customer (only `metadata`, no
`email`/`name`). A separate cleanup should pass through the Velox
customer's email + display name so the Stripe dashboard shows useful
identity — tracked as a follow-up, not blocking on this fix because
the bare customer is functionally correct (PM attaches, charges work,
webhook routing works).

**`off_session` is mandatory, not opt-in.** Per Stripe Checkout
semantics, `setup_future_usage='off_session'` tells the customer
their card will be saved for future charges; there is no checkbox.
Operators who want an opt-in UX would need `on_session` (saved only
if customer ticks) — but `on_session` cannot drive off-session dunning
charges, so it's the wrong primitive for an invoice-driven product.
Stripe's own Billing product uses `off_session` for the same reason.

## Alternatives considered

- **Switch to `mode=setup` + separate charge.** The /update-payment
  flow uses this. It works but adds a second roundtrip and a
  reconciler for the case where setup succeeds and charge fails —
  net more code, not less. `mode=payment` with
  `setup_future_usage` is the Stripe-recommended one-step flow for
  this scenario.
- **Always create the Stripe customer at Velox-customer-creation
  time.** Cleaner data model, but every Velox customer would have a
  Stripe customer record even if they're never charged via Stripe
  (offline invoicing, manual reconciliation, etc.). Lazy mint on first
  Stripe-bound action keeps dormant tenants from polluting the
  operator's Stripe dashboard.

## Amendment 2026-10-11: the saved card was never recorded after migration 0097

This ADR's promise held at Stripe but not in Velox. Migration 0097 moved
cards from `customer_payment_setups` to `payment_methods`, and made the setup
upsert in `checkout.session.completed` write only the customer↔Stripe
mapping. Cards are written by the attach path, which only
`setup_intent.succeeded` called. A hosted payment confirms a PaymentIntent
with `setup_future_usage`, so no SetupIntent event comes and the card was
never recorded.

`FetchCardDetails` still made the new card Stripe's default, so Stripe and
Velox disagreed, and Velox charges the card in `payment_methods`. Found
walking FLOW I10: Stripe held two saved 4242 cards for a customer whose
Velox record had only the declining 0341. A customer with no card on file
stayed card-less, and the next invoice went to card-less dunning, asking for
a card they had just given.

**Fix.** `Stripe.RecordCheckoutCard` (shared by the webhook and the hosted
sweep) attaches the card through the payment-methods attach path. A card
already recorded as the default is skipped, so the two callers write one
audit row.

The attach follows the store's existing default rule:

- **No card on file:** the paid card becomes the default. This restores
  this ADR's "future invoices auto-charge".
- **A default card on file:** the paid card is added beside it, as Add card
  does. This matches Stripe's own default for invoice payments
  (`save_default_payment_method` off). Making the paid card the new default
  is a product choice, left for a decision. (Amended 2026-10-22: decided
  for setup and update links, where the card does become the default. A
  card paid on the hosted page is still added beside a working default.)

**Lost webhooks.** The reconciler's hosted sweep (ADR-068 amendment) now
records the card too, for a paid session whose webhooks were lost. Before,
it recorded the payment only.

## Amendment 2026-10-18: a card refused for good is replaced

[ADR-126](126-a-card-refused-for-good-is-not-retried.md) stops dunning from
charging a card the bank refused for good (lost, stolen and the rest of
Stripe's hard decline codes). Its retries charge only once another card is
the default. Under this ADR's rule, a card the customer then saved, on the
hosted page or through a setup link, was added beside the refused default,
so every retry still skipped and dunning ran out.

So the open choice above is now decided for one case. When a charge
attempt recorded a hard decline on the customer's current default card,
the card being saved becomes the default. The switch happens in the
attach's own transaction. A working default is still never replaced: after
any other decline, or none, a new card is added beside it.

## Amendment 2026-10-22: a card saved through a setup or update link becomes the default

An end-to-end Stripe sandbox walk (2026-10-22) found the rule above wrong for
setup flows. After an ordinary decline, the customer saved a new card through
the payment-update link. The success page said "we'll use it for upcoming
invoices automatically", and `FetchCardDetails` made it Stripe's default. But
Velox kept the declining card as its default, and every dunning retry charged
that card again. The same held for the operator's **Add payment method** link.

A setup or update link exists for one reason: to give Velox the card to
charge. Stripe Billing's retries charge the customer's default payment
method (`customer.invoice_settings.default_payment_method`, when the
subscription names none). Stripe also says: "When you update payment methods
after a failed payment attempt, update the field where the previous payment
failed"
([Automate payment retries, payment method ordering](https://docs.stripe.com/billing/revenue-recovery/smart-retries#payment-method-ordering), read 2026-10-22).

**Decision.** A card saved by a SetupIntent Velox created becomes the
customer's default. Velox's three Checkout setup sessions stamp
`velox_customer_id` on the SetupIntent (`setup_intent_data.metadata`): the
operator's **Add payment method** link (email or copy), the customer's
payment-update link, and legacy `POST /v1/checkout/setup`. The
`setup_intent.succeeded` handler reads that before any other lookup and asks
the attach to make the card the default. The store clears the old default
and sets the new one in the attach's transaction, on its insert path and on
its update path. The update path covers a card already saved beside, by a
redelivery or by the session's own `checkout.session.completed`. The next
dunning retry, auto charge or **Collect payment** charges it.

These keep the rule above (the first card, or one replacing a default the
bank refused for good, becomes the default; any other is added beside):

- **A card paid on the hosted page.** `RecordCheckoutCard` picks the newest
  card on the Stripe customer, a guess. In setup mode it never makes the
  card the default; the session's `setup_intent.succeeded`, which names the
  exact card, does. Whether a hosted payment's card should replace a working
  default is still open.
- **A SetupIntent Velox did not create**, matched only by its Stripe
  customer.

**Stripe's default follows Velox's.** `FetchCardDetails` no longer sets the
newest card as Stripe's default. The attach sets Stripe's
`invoice_settings.default_payment_method` whenever the saved card is Velox's
default (first card, refused-card replacement, setup flow). It runs after the
commit, best-effort, as **Set as default** does. A failure logs
and lands as `stripe_sync_error` on the audit row (ADR-053: Velox charges
the card it names; Stripe's copy changes only what the Stripe dashboard
shows).

**One attach at a time per customer.** One setup session sends both webhooks
for the same card within milliseconds. Run side by side, one insert failed on
the active-fingerprint unique index, and when that was the setup flow's
attach, the card stayed beside the old default until Stripe redelivered. The
attach now takes a per-customer advisory lock first.

**A removed card stays removed.** Stripe redelivers an event whose first
delivery failed, for up to three days, so `setup_intent.succeeded` can arrive
after the operator removed the card it names. Before, the attach brought the
row back; with this amendment it would also have made it the default, and
every retry would charge a PaymentMethod no longer attached to the Stripe
customer while the working card sat unused. Stripe does not allow that:
"Detachment is permanent and irreversible — once detached, a PaymentMethod
can no longer be used for payments or re-attached to a Customer"
([Detach a PaymentMethod](https://docs.stripe.com/api/payment_methods/detach), read 2026-10-22).
Live-mode retries run "for up to three days with an exponential backoff"
([Webhooks, automatic retries](https://docs.stripe.com/webhooks#automatic-retries), read 2026-10-22).
The attach now leaves a removed row untouched and changes no default. The same
holds for an older copy of a card that dedupe-by-fingerprint replaced.

**The audit log shows the change.** When an attach moves the default from
another card, a `default_changed` row (trigger `setup_link`, or
`replaced_refused_default` for ADR-126) follows the `attached` row, as
**Set as default** and Remove's auto-promote write. The `attached` row's
`is_default` says whether the card is the default after the attach.

**Behavior change.** An operator's **Add payment method** link now replaces
a working default. Before, the card was added beside it.

**Left as they are.**

- A `setup_intent.succeeded` whose first delivery failed, redelivered after
  a later default choice (the operator's **Set as default**, or a second
  link's card), makes its card the default again. Event-id dedup does not
  help: it only skips a redelivery after a success. It takes a delivery
  that failed for this event alone and a default change inside Stripe's
  retry window, and the card charged is one the customer gave through a
  link and that is still attached. Ordering by when the default was set
  would need a new column, and Stripe warns against ordering by the
  event's `created` ("Don't use `created` to determine event order",
  [Webhooks, event ordering](https://docs.stripe.com/webhooks#event-ordering),
  read 2026-10-22). Not built until this is seen.
- A **Set as default** that runs during a setup attach can fail on the
  one-default index. It fails loudly; there are never two defaults.
- No retry runs when the card is saved. The next scheduled retry, or
  **Collect payment**, charges it.
