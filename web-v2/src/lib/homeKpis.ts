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

// What each Home card counts, shown under the number on the card.
export const CUSTOMERS_DEFINITION =
  'Customers with an active or trialing subscription. Test-clock customers are not counted.'

// currentMRR (internal/analytics/overview.go): plan base fee × quantity of
// each active subscription, a yearly plan divided by 12, in the default
// currency, test-clock subscriptions left out. Usage is not in it.
export const MRR_DEFINITION =
  'Monthly recurring revenue: the base fees of active subscriptions, per month. Usage is not included. ARR is the same × 12.'

export const FAILED_PAYMENTS_DEFINITION =
  'Invoices created in this period whose payment failed.'

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

// "2 active subscriptions · 1 trialing subscription": the subscriptions
// behind the customer count, each count with its own noun. Trialing shows
// only when there is one.
export function subscriptionsHint(o: HomeOverviewCounts): string {
  const active = subscriptions(count(o.active_subscriptions), 'active')
  const trialing = count(o.trialing_subscriptions)
  return trialing > 0 ? `${active} · ${subscriptions(trialing, 'trialing')}` : active
}

function subscriptions(n: number, kind: string): string {
  return `${n} ${kind} ${n === 1 ? 'subscription' : 'subscriptions'}`
}
