// One status vocabulary for the whole dashboard.
//
// The same invoice used to read "Open" (tab), "finalized" (badge),
// "Showing finalized" (subtitle) and "pending" (payment badge) on one
// screen. The invoice badges (invoice list, invoice page, Home and the
// customer page's recent invoices) go through these functions, so an
// invoice state has one name and one colour. Other badges still use
// lib/status; move them here when you touch them.

export type StatusTone = 'neutral' | 'info' | 'success' | 'warning' | 'danger'
export interface StatusDisplay { label: string; tone: StatusTone }

// humanizeStatus turns a raw code into sentence case: "no_endpoints" →
// "No endpoints", "past_due" → "Past due". The fallback for codes without
// an explicit label below.
export function humanizeStatus(code: string): string {
  const s = code.replace(/[_-]+/g, ' ').trim()
  return s ? s[0].toUpperCase() + s.slice(1).toLowerCase() : ''
}

export interface InvoiceStatusInput {
  status: string
  payment_status?: string
  due_at?: string | null
}

// invoiceStatus is the single badge for an invoice: lifecycle and payment
// folded together, as Stripe shows them (Draft, Open, Past due, Paid, Void,
// Uncollectible, plus Processing and Payment failed while a charge is in
// flight or has failed). `nowMs` is the entity's effective now (test clock
// aware) and only decides Past due.
export function invoiceStatus(inv: InvoiceStatusInput, nowMs: number): StatusDisplay {
  switch (inv.status) {
    case 'draft': return { label: 'Draft', tone: 'neutral' }
    case 'voided': return { label: 'Void', tone: 'neutral' }
    case 'uncollectible': return { label: 'Uncollectible', tone: 'danger' }
    case 'paid': return { label: 'Paid', tone: 'success' }
  }
  if (inv.payment_status === 'succeeded') return { label: 'Paid', tone: 'success' }
  if (inv.payment_status === 'processing') return { label: 'Processing', tone: 'info' }
  if (inv.payment_status === 'failed') return { label: 'Payment failed', tone: 'danger' }
  if (inv.status === 'finalized') {
    const due = inv.due_at ? new Date(inv.due_at).getTime() : NaN
    if (!Number.isNaN(due) && due < nowMs) return { label: 'Past due', tone: 'danger' }
    return { label: 'Open', tone: 'info' }
  }
  return { label: humanizeStatus(inv.status), tone: 'neutral' }
}

// invoicePaymentChip is the payment chip shown beside the invoice badge, or
// null when it would only repeat the badge. Drafts have no payment yet; Paid
// and Void say all there is to say; and when the badge already reads Paid,
// Processing or Payment failed, a "Succeeded"/"Processing"/"Failed" chip next
// to it is the same fact twice. What is left is the payment state the badge
// does not show (for example Pending beside Open, or beside Uncollectible).
export function invoicePaymentChip(inv: InvoiceStatusInput): StatusDisplay | null {
  if (inv.status === 'draft' || inv.status === 'paid' || inv.status === 'voided') return null
  if (!inv.payment_status) return null
  if (inv.status === 'finalized' && PAYMENT_IN_BADGE.has(inv.payment_status)) return null
  return genericStatus(inv.payment_status)
}

// The payment states invoiceStatus folds into the badge of a finalized invoice.
const PAYMENT_IN_BADGE = new Set(['succeeded', 'processing', 'failed'])

export interface SubscriptionStatusInput {
  status: string
  pause_collection?: unknown
  cancel_at_period_end?: boolean
  cancel_at?: string | null
}

// subscriptionStatus is the main badge for a subscription.
export function subscriptionStatus(sub: SubscriptionStatusInput): StatusDisplay {
  switch (sub.status) {
    case 'active': return { label: 'Active', tone: 'success' }
    case 'trialing': return { label: 'Trialing', tone: 'info' }
    case 'canceled': return { label: 'Canceled', tone: 'neutral' }
    case 'draft': return { label: 'Draft', tone: 'neutral' }
    case 'archived': return { label: 'Archived', tone: 'neutral' }
  }
  return { label: humanizeStatus(sub.status), tone: 'neutral' }
}

// subscriptionFlags are the secondary chips shown beside the main badge, for
// states the main status does not carry: collection paused (often by
// dunning) and a scheduled cancel. Only for live subscriptions.
export function subscriptionFlags(sub: SubscriptionStatusInput): StatusDisplay[] {
  if (sub.status !== 'active' && sub.status !== 'trialing') return []
  const flags: StatusDisplay[] = []
  if (sub.pause_collection) flags.push({ label: 'Collection paused', tone: 'warning' })
  if (sub.cancel_at_period_end || sub.cancel_at) flags.push({ label: 'Cancel scheduled', tone: 'warning' })
  return flags
}

// genericStatus covers everything else (customers, dunning runs, webhook
// deliveries, API keys, test clocks): a known tone per code and a
// sentence-case label.
const TONES: Record<string, StatusTone> = {
  active: 'success', succeeded: 'success', delivered: 'success', resolved: 'success', ready: 'success', recovered: 'success',
  pending: 'warning', paused: 'warning', escalated: 'warning', advancing: 'info', processing: 'info', trialing: 'info',
  failed: 'danger', revoked: 'danger', expired: 'danger', internal_failure: 'danger', disabled: 'neutral',
  archived: 'neutral', canceled: 'neutral', draft: 'neutral', skipped: 'neutral', no_endpoints: 'neutral',
}
const LABELS: Record<string, string> = {
  no_endpoints: 'No endpoint',
  internal_failure: 'Failed',
}

export function genericStatus(code: string): StatusDisplay {
  return { label: LABELS[code] ?? humanizeStatus(code), tone: TONES[code] ?? 'neutral' }
}
