// The plans a price override is most likely for: those of the customer's
// subscriptions that can still bill (all but canceled ones), including a
// plan an item is scheduled to move to. The override form lists their prices
// first. A price of any other plan can still be picked: the override applies
// if the customer joins that plan later. Plain data so the unit tests can
// check it.

export function customerPlanIds(
  subscriptions: { status: string; items?: { plan_id: string; pending_plan_id?: string }[] }[],
): Set<string> {
  const ids = new Set<string>()
  for (const s of subscriptions) {
    if (s.status === 'canceled') continue
    for (const it of s.items ?? []) {
      ids.add(it.plan_id)
      if (it.pending_plan_id) ids.add(it.pending_plan_id)
    }
  }
  return ids
}

// customerPlansFirst keeps the order within each group and puts the prices
// of the customer's plans before the others.
export function customerPlansFirst<T>(items: T[], planId: (item: T) => string, mine: Set<string>): T[] {
  return [...items.filter(i => mine.has(planId(i))), ...items.filter(i => !mine.has(planId(i)))]
}

export const CUSTOMER_PLANS_FIRST_HINT = "Prices of this customer's plans are listed first."
