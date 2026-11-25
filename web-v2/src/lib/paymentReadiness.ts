import type { Subscription, CustomerPaymentMethod } from '@/lib/api'

// A customer "needs a payment method" when they have no card on file yet
// have a subscription that will try to auto-charge one. Hardcount has no
// collection_method concept — every active, non-paused
// subscription is auto-charge by definition — so this is simply "a
// subscription that will attempt a charge against a card that isn't there".
//
// For usage-based billing the amount is unknown until period close, so a
// silent no-card subscription is a guaranteed future failed charge. This
// derives the shift-left warning purely on the frontend from data the
// customer/subscription pages already load — no backend signal needed
// (2026-07-22 payment-surfacing audit, P1-5).
//
// It is disjoint from the REACTIVE no-payment-method invoice banner: this
// fires on the customer/subscription surfaces BEFORE any invoice exists;
// the banner fires on a finalized invoice. The two never co-render.

export type AutoChargeFields = Pick<Subscription, 'status' | 'pause_collection'>

// A scheduled cancel does not stop the warning: the period before it is
// still billed.
export function subscriptionWillAutoCharge(sub: AutoChargeFields): boolean {
  switch (sub.status) {
    // A paused-collection subscription won't attempt a charge — no warning.
    case 'active': return !sub.pause_collection
    case 'canceled': return false
  }
}

export function hasUsableCard(paymentMethods: Pick<CustomerPaymentMethod, 'is_default'>[]): boolean {
  return paymentMethods.some(pm => pm.is_default)
}

// customerNeedsPaymentMethod: zero cards on file AND ≥1 auto-charging sub.
export function customerNeedsPaymentMethod(
  subscriptions: AutoChargeFields[],
  paymentMethods: Pick<CustomerPaymentMethod, 'is_default'>[],
): boolean {
  if (hasUsableCard(paymentMethods)) return false
  return subscriptions.some(subscriptionWillAutoCharge)
}
