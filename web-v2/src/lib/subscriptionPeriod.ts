// The dates a subscription page and list show, from one place. The summary,
// the timeline, the list's "Next bill" column and the cancel dialog all read
// them here, so a canceled subscription never says "Renews", and one boundary
// never shows as two dates ("Period end Nov 15" beside "Cancels Nov 16").
//
// The convention, everywhere:
//   - a period's last covered day is "through Nov 15" (inclusive);
//   - the boundary after it is the event day: "Renews Nov 16", "Won't renew
//     Nov 16", "Cancels Nov 16".
// Dates come in through `fmt`, so tests/subscriptionPeriod.test.ts can check
// every branch.

import { hasScheduledCancel } from './subscriptionNotices.ts'
import type { SubscriptionStatus } from './gen/schemas/subscriptionStatus'

export interface SubscriptionPeriodInput {
  status: SubscriptionStatus
  current_billing_period_start?: string
  current_billing_period_end?: string
  next_billing_at?: string
  cancel_at_period_end?: boolean
  cancel_at?: string | null
  cancel_effective_at?: string
  canceled_at?: string
  pause_collection?: { resumes_at?: string } | null
}

export interface PeriodDateFormat {
  // An event instant as a calendar date ("Nov 16, 2026").
  date: (iso: string) => string
  // The inclusive last day of a half-open period end ("Nov 15, 2026").
  lastDay: (endISO: string) => string
}

// What happens next, as one label and one value: the summary cell, the
// list's "Next bill" cell and the header meta all read this.
export interface NextEvent {
  label: string
  value: string
  // 'ended' greys the value in a list; 'scheduled' marks a pending cancel.
  tone: 'normal' | 'scheduled' | 'ended'
}

export interface PeriodLabels {
  // Last covered day of the current period ("Nov 15, 2026"). Null when there
  // is no current period.
  accessThrough: string | null
  // The renewal day, only when the subscription will renew.
  renewsOn: string | null
  // The day a scheduled cancel takes effect.
  cancelsOn: string | null
  // The day it was canceled.
  canceledOn: string | null
  next: NextEvent | null
}

export function periodLabels(sub: SubscriptionPeriodInput, fmt: PeriodDateFormat): PeriodLabels {
  const live = sub.status === 'active'
  const canceled = sub.status === 'canceled'
  const scheduled = live && hasScheduledCancel(sub)

  const accessThrough = live && sub.current_billing_period_end ? fmt.lastDay(sub.current_billing_period_end) : null

  // cancel_effective_at is the server's answer: cancel_at when set, else the
  // end of the current period.
  const cancelAt = scheduled ? (sub.cancel_effective_at || sub.cancel_at || sub.current_billing_period_end) : undefined
  const cancelsOn = cancelAt ? fmt.date(cancelAt) : null

  const renewAt = live && !scheduled ? (sub.next_billing_at || sub.current_billing_period_end) : undefined
  const renewsOn = renewAt ? fmt.date(renewAt) : null

  const canceledOn = canceled && sub.canceled_at ? fmt.date(sub.canceled_at) : null

  let next: NextEvent | null = null
  if (canceled) {
    if (canceledOn) next = { label: 'Canceled', value: canceledOn, tone: 'ended' }
  } else if (cancelsOn) {
    next = { label: 'Cancels', value: cancelsOn, tone: 'scheduled' }
  } else if (renewsOn) {
    next = { label: 'Renews', value: renewsOn, tone: 'normal' }
  }

  return { accessThrough, renewsOn, cancelsOn, canceledOn, next }
}

// The "At period end" choice in the cancel dialog, and the scheduled-cancel
// notice: the last day with access, then the renewal that won't happen.
export function cancelAtPeriodEndText(labels: Pick<PeriodLabels, 'accessThrough' | 'cancelsOn' | 'renewsOn'>): string {
  const boundary = labels.cancelsOn ?? labels.renewsOn
  if (labels.accessThrough && boundary) {
    return `The customer keeps access through ${labels.accessThrough} and the subscription doesn't renew on ${boundary}. You can undo this until then.`
  }
  return 'The customer keeps access until the current period ends. You can undo this until then.'
}

// A list cell: "Renews Nov 16, 2026", "Ended Oct 16, 2026", "Cancels Nov 16, 2026".
export function nextBillCell(labels: PeriodLabels): { text: string; tone: NextEvent['tone'] } | null {
  if (!labels.next) return null
  if (labels.next.tone === 'ended') return { text: `Ended ${labels.next.value}`, tone: 'ended' }
  return { text: `${labels.next.label} ${labels.next.value}`, tone: labels.next.tone }
}
