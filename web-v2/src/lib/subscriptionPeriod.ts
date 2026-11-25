// The dates a subscription page and list show, from one place. The summary,
// the timeline and the list's "Next bill" column all read them here, so a
// canceled subscription never says "Renews".
//
// A renewal is dated by the period boundary instant ("Renews Nov 16"), not
// by the period's last covered day. Dates come in through `date`, so
// tests/subscriptionPeriod.test.ts can check every branch.

import type { SubscriptionStatus } from './gen/schemas/subscriptionStatus'

export interface SubscriptionPeriodInput {
  status: SubscriptionStatus
  current_billing_period_start?: string
  current_billing_period_end?: string
  next_billing_at?: string
  canceled_at?: string
  pause_collection?: { resumes_at?: string } | null
}

// What happens next, as one label and one value: the summary cell, the
// Properties card and the list's "Next bill" cell all read this.
export interface NextEvent {
  label: string
  value: string
  // 'ended' on a canceled subscription: the list cell reads "Ended …".
  tone: 'normal' | 'ended'
}

export interface PeriodLabels {
  // The renewal day, only when the subscription will renew.
  renewsOn: string | null
  // The day it was canceled.
  canceledOn: string | null
  next: NextEvent | null
}

// periodLabels reads one subscription's dates. `date` formats an event
// instant as a calendar date ("Nov 16, 2026").
export function periodLabels(sub: SubscriptionPeriodInput, date: (iso: string) => string): PeriodLabels {
  const live = sub.status === 'active'
  const canceled = sub.status === 'canceled'

  const renewAt = live ? (sub.next_billing_at || sub.current_billing_period_end) : undefined
  const renewsOn = renewAt ? date(renewAt) : null

  const canceledOn = canceled && sub.canceled_at ? date(sub.canceled_at) : null

  let next: NextEvent | null = null
  if (canceledOn) next = { label: 'Canceled', value: canceledOn, tone: 'ended' }
  else if (renewsOn) next = { label: 'Renews', value: renewsOn, tone: 'normal' }

  return { renewsOn, canceledOn, next }
}

// A list cell: "Renews Nov 16, 2026", "Ended Oct 16, 2026".
export function nextBillCell(labels: PeriodLabels): string | null {
  if (!labels.next) return null
  if (labels.next.tone === 'ended') return `Ended ${labels.next.value}`
  return `${labels.next.label} ${labels.next.value}`
}
