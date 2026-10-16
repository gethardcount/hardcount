// The Home page KPI numbers and the baselines their trends compare against.
// Plain functions so Vitest can check them.
//
// Rule: a trend compares two numbers the server computed with the same
// definition. The page used to build baselines itself: Failed payments
// against a hard-coded 0, and Active customers as "active now minus new in
// the window", which went negative (archived customers count as new but not
// as active). The server now returns *_prev for each.

export interface HomeOverviewCounts {
  live_customers: number
  live_customers_prev: number
  active_subscriptions: number
  trialing_subscriptions: number
  failed_payments: number
  failed_payments_prev: number
}

export interface Trend {
  current: number
  previous: number
}

// What "Customers" means on Home. Shown as the card's tooltip.
export const CUSTOMERS_DEFINITION =
  'Customers with at least one active or trialing subscription. Customers on a test clock are not counted.'

export const FAILED_PAYMENTS_DEFINITION =
  'Invoices created in this period whose payment failed, compared with the period before.'

// Never negative: a count below zero is a bug, and a negative baseline turns
// the percentage upside down.
function count(n: number | undefined | null): number {
  return typeof n === 'number' && Number.isFinite(n) && n > 0 ? Math.floor(n) : 0
}

export function customersTrend(o: HomeOverviewCounts): Trend {
  return { current: count(o.live_customers), previous: count(o.live_customers_prev) }
}

export function failedPaymentsTrend(o: HomeOverviewCounts): Trend {
  return { current: count(o.failed_payments), previous: count(o.failed_payments_prev) }
}

// "3 active · 1 trialing subscriptions": the subscriptions behind the
// customer count. Trialing shows only when there is one.
export function subscriptionsHint(o: HomeOverviewCounts): string {
  const active = count(o.active_subscriptions)
  const trialing = count(o.trialing_subscriptions)
  const noun = active + trialing === 1 ? 'subscription' : 'subscriptions'
  return trialing > 0 ? `${active} active · ${trialing} trialing ${noun}` : `${active} active ${noun}`
}
