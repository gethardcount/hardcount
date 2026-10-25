// Words for the plan, meter and price pages that depend on state. Plain
// functions so the unit tests can check each version.

// "1 subscription uses" / "3 subscriptions use".
function subscriptionsUse(n: number): string {
  return n === 1 ? '1 subscription uses' : `${n.toLocaleString('en-US')} subscriptions use`
}

export const PLAN_LOCKED_TITLE = 'Plan in use'

// The plan lock (ADR-034): the server refuses a base fee change while any
// subscription that is not canceled uses the plan (pricing.Service.UpdatePlan).
// `liveSubscriptions` is the server's own count of those (GET /v1/plans/{id}
// live_subscription_count), so the page and the refusal agree. Same sentence
// as the refusal.
export function planLockedSentence(liveSubscriptions: number): string {
  return `${subscriptionsUse(liveSubscriptions)} this plan, so its base fee is fixed. Create a new plan with the new fee instead.`
}

// When a change to a plan's usage prices applies (ADR-130 §5), said before
// the operator saves.
export function planPricesNotice(used: boolean): string {
  return used
    ? "Price changes apply from each subscriber's next billing period. A billing period uses the prices the plan had at its start, usually midnight on its first day. So a subscription that starts later today, or that moves to this plan or adds it partway through its period, gets a change from its next period. A move from a plan with another billing interval starts a new period then, so it gets a change at once."
    : 'No one has subscribed to this plan yet, so price changes apply at once.'
}

// The toast after a price write, from the server's own answer.
export function priceWriteToast(w: { effective: 'now' | 'next_period'; removed?: boolean; ended_overrides?: unknown[] }): string {
  const what = w.removed ? 'Price removed' : 'Price saved'
  const when = w.effective === 'now' ? '' : ", from each subscriber's next billing period"
  const ended = w.ended_overrides?.length ?? 0
  const overrides = ended > 0 ? ` ${ended === 1 ? '1 customer override' : `${ended.toLocaleString('en-US')} customer overrides`} ended.` : ''
  return `${what}${when}.${overrides}`
}

// The confirm for removing a price. `overrides` is the number of active
// customer overrides the removal ends, or null when they could not be read.
export function priceRemoveSentence(used: boolean, overrides: number | null): string {
  // The events it took go to the plan's next most specific price for them.
  const when = used
    ? "From each subscriber's next billing period, its events go to another price of the plan that matches them, or aren't billed if none does. A billing period uses the prices the plan had at its start, usually midnight on its first day. So a subscription that starts later today, or that moves to this plan or adds it partway through its period, still pays this price until its next period. A move from a plan with another billing interval starts a new period then, so it doesn't pay this price."
    : "It is removed at once. Its events go to another price of the plan that matches them, or aren't billed if none does."
  const ends = overrides === null
    ? ' It also ends any customer override of it.'
    : overrides === 0
      ? ''
      : ` It also ends ${overrides === 1 ? '1 customer override' : `${overrides.toLocaleString('en-US')} customer overrides`} of it.`
  return `${when}${ends} Adding the price back later doesn't bring the overrides back.`
}
