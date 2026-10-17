# ADR-021: Hosted-invoice Pay flow saves the card to the customer's Stripe customer

**Status:** Accepted (amended 2026-10-11)
**Date:** 2026-05-03

## Summary

Paying on the hosted invoice page saves the card for later off-session charges. The Stripe Checkout session names the customer's Stripe customer (created on first use) and sets `setup_future_usage=off_session`, so future invoices can charge the card without the customer present. Amended 2026-10-11: Velox also records that card in its own `payment_methods`, through `Stripe.RecordCheckoutCard`, from the webhook or the reconciler's hosted sweep. The card becomes the default only when the customer had no card on file. Status: active, amended.

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
  is a product choice, left for a decision.

**Lost webhooks.** The reconciler's hosted sweep (ADR-068 amendment) now
records the card too, for a paid session whose webhooks were lost. Before,
it recorded the payment only.
