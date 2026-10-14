// Words for a saved card and for removing one. Plain data so node --test can
// check each branch.

export function cardLabel(pm: { card_brand?: string | null; card_last4?: string | null }): string {
  const brand = pm.card_brand || 'card'
  return `${brand.charAt(0).toUpperCase() + brand.slice(1)} ····${pm.card_last4 || '????'}`
}

// The remove-card confirm. Removing a card also removes it at Stripe. When
// it is the only card, new invoices can't be charged until the customer adds
// another one. When other cards remain and this one is the default, the
// server makes the newest remaining card the default in the same step
// (paymentmethods.Service.Detach), so charging carries on.
//
// It no longer promises the customer "will be emailed a link to add a
// card": that email goes out only when the customer has an address and
// email sending is set up.
export function removeCardSentence(o: { onlyCard: boolean; isDefault: boolean }): string {
  if (o.onlyCard) {
    return "This is the customer's only card. New invoices won't be charged automatically until they add another one."
  }
  if (o.isDefault) {
    return 'The card is also removed in Stripe. The newest remaining card becomes the default.'
  }
  return 'The card is also removed in Stripe. The default card stays the same.'
}
