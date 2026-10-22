// The Subscriptions list tabs and the API filter each one sends. Most tabs
// are a status, but two are not: paused collection (the subscription stays
// active and keeps billing into drafts) and failing billing are their own
// server filters. The Paused tab once sent ?status=paused, which no
// subscription has, so it was always empty.

export const SUBSCRIPTION_TABS = [
  { value: '', label: 'All' },
  { value: 'active', label: 'Active' },
  { value: 'paused', label: 'Collection paused' },
  { value: 'canceled', label: 'Canceled' },
  { value: 'draft', label: 'Draft' },
  { value: 'billing_failing', label: 'Billing failing' },
] as const

export interface SubscriptionListParams {
  status?: string
  collection?: string
  billing?: string
}

// subscriptionTabParams is the query a tab sends to GET /v1/subscriptions.
export function subscriptionTabParams(tab: string): SubscriptionListParams {
  if (tab === '') return {}
  if (tab === 'paused') return { collection: 'paused' }
  if (tab === 'billing_failing') return { billing: 'failing' }
  return { status: tab }
}

// subscriptionTabEmptyTitle is the empty-state title when a tab has no rows.
export function subscriptionTabEmptyTitle(tab: string): string {
  if (tab === 'paused') return 'No subscriptions with collection paused'
  if (tab === 'billing_failing') return 'No subscriptions with failing billing'
  return `No ${tab} subscriptions`
}
