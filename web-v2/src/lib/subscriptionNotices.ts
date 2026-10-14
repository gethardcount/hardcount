// What the subscription page shows under its header, and the copy that
// depends on real state. Plain data in, plain data out, so node --test can
// check every branch without rendering.

// Up to four notices can apply to one subscription. The page shows at most
// one notice box (the most urgent present); the rest become one-line rows
// under the header, each keeping its own button. Order, most urgent first:
//   1. billing failure: no invoice is being written, money is not billed;
//   2. no payment method: renewals will be billed but can't be charged;
//   3. scheduled cancel: billing ends on a date the operator chose;
//   4. collection paused: a choice the operator made, invoices stay drafts.
export type SubscriptionNoticeKind = 'billing_failure' | 'no_card' | 'cancel_scheduled' | 'collection_paused'

export const SUBSCRIPTION_NOTICE_ORDER: readonly SubscriptionNoticeKind[] = [
  'billing_failure',
  'no_card',
  'cancel_scheduled',
  'collection_paused',
]

export interface SubscriptionNoticeSignals {
  billingFailure: boolean
  needsCard: boolean
  cancelScheduled: boolean
  collectionPaused: boolean
}

export interface SubscriptionNoticeLayout {
  // The one notice drawn as a box, or null when none applies.
  box: SubscriptionNoticeKind | null
  // The others that apply, as one-line rows, in order.
  rows: SubscriptionNoticeKind[]
}

export function subscriptionNoticeLayout(s: SubscriptionNoticeSignals): SubscriptionNoticeLayout {
  const present: Record<SubscriptionNoticeKind, boolean> = {
    billing_failure: s.billingFailure,
    no_card: s.needsCard,
    cancel_scheduled: s.cancelScheduled,
    collection_paused: s.collectionPaused,
  }
  const shown = SUBSCRIPTION_NOTICE_ORDER.filter(k => present[k])
  return { box: shown[0] ?? null, rows: shown.slice(1) }
}

// A scheduled cancel is either the at-period-end flag or an explicit date.
export function hasScheduledCancel(sub: { cancel_at_period_end?: boolean; cancel_at?: string | null }): boolean {
  return !!(sub.cancel_at_period_end || sub.cancel_at)
}

// The billing-failure notice. The attempt count goes in the title; the
// server's reason is the sentence; the next try or the stop follows it.
export function billingFailureTitle(f: { parked: boolean; attempts: number; max_attempts: number }): string {
  if (f.parked) return 'Billing has stopped'
  return `Billing failed (attempt ${f.attempts} of ${f.max_attempts})`
}

// The line after the server's reason. nextAttemptLabel is the formatted
// next_attempt_at, or undefined when the server sent none.
export function billingFailureNextLine(f: { parked: boolean; attempts: number }, nextAttemptLabel?: string): string | null {
  if (f.parked) {
    return `It failed ${f.attempts} ${f.attempts === 1 ? 'time' : 'times'}. No invoice is created until you retry.`
  }
  if (nextAttemptLabel) return `Next automatic try after ${nextAttemptLabel}.`
  return null
}

// The no-card notice, the same words on the customer and subscription
// pages. It used to promise the next invoice "will need manual follow-up",
// which is false: a setup link is emailed when the invoice is finalized,
// and the invoice is charged once a card is added.
export const NO_CARD_TITLE = 'No payment method'
export const NO_CARD_SENTENCE = "Renewals can't be charged until the customer adds a card."

// The scheduled-cancel sentence. cancel_effective_at is the server's own
// answer (ADR-069): on a trialing subscription the flag cancels at trial end,
// free. effectiveLabel is the formatted cancel_effective_at.
export function cancelScheduledSentence(
  sub: { status: string; cancel_at_period_end?: boolean },
  effectiveLabel?: string,
): string | null {
  if (!effectiveLabel) return null
  if (sub.status === 'trialing' && sub.cancel_at_period_end) {
    return `At trial end (${effectiveLabel}); the customer won't be charged.`
  }
  return `${sub.cancel_at_period_end ? 'At end of current period' : 'On'} ${effectiveLabel}.`
}

// The collection-paused sentence. resumesLabel is the formatted resumes_at.
// It must not read as "charged once resumed": a draft made while paused
// stays a draft after the resume (subscription.Service.ResumeCollection).
export function collectionPausedSentence(resumesLabel?: string): string {
  return resumesLabel
    ? `New invoices are saved as drafts and not charged. Collection resumes ${resumesLabel}.`
    : 'New invoices are saved as drafts and not charged.'
}
