// One status vocabulary for the whole dashboard.
//
// The same invoice used to read "Open" (tab), "finalized" (badge),
// "Showing finalized" (subtitle) and "pending" (payment badge) on one
// screen. Every status badge goes through these functions and StatusBadge,
// so a state has one name and one colour on every page; no page prints a raw
// code ("active", "dispatched") or picks its own colour.

import { isTerminalInvoiceStatus } from './invoiceTerminal.ts'

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
// null when it would only repeat the badge. Drafts have no payment yet; Paid,
// Void and Uncollectible say all there is to say (collection has ended, so
// "Pending" or "Failed" beside a write-off read as a payment still expected),
// except "Unknown" on a write-off: a parked charge can be written off and is
// marked paid later if it succeeded (docs/payments.md), so money may still
// arrive;
// and when the badge already reads Paid, Processing or Payment failed, a
// "Succeeded"/"Processing"/"Failed" chip next to it is the same fact twice.
// What is left is the payment state the badge does not show (Pending or
// Unknown beside Open).
export function invoicePaymentChip(inv: InvoiceStatusInput): StatusDisplay | null {
  if (inv.status === 'uncollectible') return inv.payment_status === 'unknown' ? genericStatus('unknown') : null
  if (inv.status === 'draft' || isTerminalInvoiceStatus(inv.status)) return null
  if (!inv.payment_status) return null
  if (inv.status === 'finalized' && PAYMENT_IN_BADGE.has(inv.payment_status)) return null
  return genericStatus(inv.payment_status)
}

// The payment states invoiceStatus folds into the badge of a finalized invoice.
const PAYMENT_IN_BADGE = new Set(['succeeded', 'processing', 'failed'])

export interface SubscriptionStatusInput {
  status: string
  pause_collection?: { behavior?: string; resumes_at?: string | null } | null
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
  }
  return { label: humanizeStatus(sub.status), tone: 'neutral' }
}

// collectionPausedFlag is the chip a list shows beside a live subscription
// whose collection is paused (by an operator or by dunning), or null. The
// same rule as GET /v1/subscriptions?collection=paused: live means active or
// trialing (a canceled row can keep the pause columns), and a timed pause
// ends at resumes_at, though the row can keep it a while after. `nowMs` is
// the subscription's effective now (test clock aware).
export function collectionPausedFlag(sub: SubscriptionStatusInput, nowMs: number): StatusDisplay | null {
  if (sub.status !== 'active' && sub.status !== 'trialing') return null
  const pause = sub.pause_collection
  if (!pause) return null
  if (pause.resumes_at && new Date(pause.resumes_at).getTime() <= nowMs) return null
  return { label: 'Collection paused', tone: 'warning' }
}

// trialBadgeEndsAt is the end the "Trial ends" badge counts down to, or null
// when no trial badge belongs on the subscription. trial_end_at stays on the
// record after the trial ends (or is ended now), so a badge read from it alone
// showed a red "Expired" on active, paying subscriptions (sweep E5).
export function trialBadgeEndsAt(sub: { status: string; trial_end_at?: string | null }): string | null {
  return sub.status === 'trialing' && sub.trial_end_at ? sub.trial_end_at : null
}

// subscriptionFlags are the secondary chips shown beside the main badge, for
// states the main status does not carry: collection paused (often by
// dunning) and a scheduled cancel. Only for live subscriptions.
export function subscriptionFlags(sub: SubscriptionStatusInput, nowMs: number): StatusDisplay[] {
  if (sub.status !== 'active' && sub.status !== 'trialing') return []
  const flags: StatusDisplay[] = []
  const paused = collectionPausedFlag(sub, nowMs)
  if (paused) flags.push(paused)
  if (sub.cancel_at_period_end || sub.cancel_at) flags.push({ label: 'Cancel scheduled', tone: 'warning' })
  return flags
}

// dunningRunStatus is a dunning run's badge. An active run is a payment
// being chased, so it is amber, not the green "active" means elsewhere; an
// escalated one ran out of retries. The words match the Dunning page's tabs.
export function dunningRunStatus(state: string): StatusDisplay {
  switch (state) {
    case 'active': return { label: 'Active', tone: 'warning' }
    case 'escalated': return { label: 'Escalated', tone: 'danger' }
    case 'resolved': return { label: 'Resolved', tone: 'success' }
  }
  return { label: humanizeStatus(state), tone: 'neutral' }
}

// sentEmailStatus is a sent email's badge on the customer page: what the
// mail provider reported when it did (delivered, bounced, marked as spam),
// else where the email is in our outbox. "Dispatched" is our word for handed
// to the mail server, so the customer-facing reader sees "Sent".
export function sentEmailStatus(em: { status: string; delivery_state?: string }): StatusDisplay {
  switch (em.delivery_state) {
    case 'delivered': return { label: 'Delivered', tone: 'success' }
    case 'bounced': return { label: 'Bounced', tone: 'danger' }
    case 'complained': return { label: 'Marked as spam', tone: 'danger' }
  }
  switch (em.status) {
    case 'pending': return { label: 'Queued', tone: 'neutral' }
    case 'dispatched': return { label: 'Sent', tone: 'info' }
    case 'failed': return { label: 'Failed', tone: 'danger' }
    case 'skipped': return { label: 'Not sent: settled first', tone: 'neutral' }
  }
  return { label: humanizeStatus(em.delivery_state && em.delivery_state !== 'unknown' ? em.delivery_state : em.status), tone: 'neutral' }
}

// genericStatus covers everything else (customers, plans, credit notes,
// invitations, webhook deliveries, API keys, test clocks): a known tone per
// code and a sentence-case label.
const TONES: Record<string, StatusTone> = {
  active: 'success', succeeded: 'success', delivered: 'success', resolved: 'success', ready: 'success', recovered: 'success',
  pending: 'warning', paused: 'warning', escalated: 'warning', advancing: 'info', processing: 'info', trialing: 'info',
  failed: 'danger', revoked: 'danger', expired: 'danger', internal_failure: 'danger', disabled: 'neutral',
  archived: 'neutral', canceled: 'neutral', draft: 'neutral', skipped: 'neutral', no_endpoints: 'neutral',
  issued: 'info', accepted: 'success', voided: 'neutral',
}
const LABELS: Record<string, string> = {
  no_endpoints: 'No endpoint',
  internal_failure: 'Failed',
  voided: 'Void',
}

export function genericStatus(code: string): StatusDisplay {
  return { label: LABELS[code] ?? humanizeStatus(code), tone: TONES[code] ?? 'neutral' }
}

// webhookStatus is a webhook event's or delivery's badge. An event rolls its
// deliveries up (pending, failed, delivered) or no endpoint listens for it
// (no_endpoints); a delivery is pending, succeeded or failed. A pending
// delivery is "Sending" before its first try and "Retrying" after one failed;
// an event can't tell which, so it is "In progress". Raw codes used to show
// ("succeeded", "pending").
export function webhookStatus(status: string, tried?: boolean): StatusDisplay {
  switch (status) {
    case 'delivered':
    case 'succeeded': return { label: 'Delivered', tone: 'success' }
    case 'pending':
      if (tried === undefined) return { label: 'In progress', tone: 'info' }
      return tried ? { label: 'Retrying', tone: 'warning' } : { label: 'Sending', tone: 'info' }
    case 'failed': return { label: 'Failed', tone: 'danger' }
    case 'no_endpoints': return { label: 'Not sent', tone: 'neutral' }
  }
  return genericStatus(status)
}
