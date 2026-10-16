// The dates a subscription page and list show, from one place.
//
// The summary, the timeline, the list's "Next bill" column and the cancel
// dialog each used to work out "renews" on their own, so a canceled
// subscription still said "Renews Nov 16", an immediate cancel in the middle
// of a trial read "Canceled at trial end", and one page said both "Period end
// Nov 15" and "Cancels Nov 16 12:00 AM" for the same boundary (M-SUB-3/4/5).
//
// The convention, everywhere:
//   - a period's last covered day is "through Nov 15" (inclusive);
//   - the boundary after it is the event day: "Renews Nov 16", "Won't renew
//     Nov 16", "Cancels Nov 16".
// Dates come in through `fmt`, so node --test can check every branch.

import { hasScheduledCancel } from './subscriptionNotices.ts'

export interface SubscriptionPeriodInput {
  status: string
  current_billing_period_start?: string
  current_billing_period_end?: string
  next_billing_at?: string
  trial_start_at?: string
  trial_end_at?: string
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
  // Last covered day of the current period ("Nov 15, 2026"); for a trial,
  // the trial's last day. Null when there is no current period.
  accessThrough: string | null
  // The renewal day, only when the subscription will renew.
  renewsOn: string | null
  // The day a scheduled cancel takes effect.
  cancelsOn: string | null
  // The day it was canceled.
  canceledOn: string | null
  trialEndsOn: string | null
  // The first paid charge of a trialing subscription with no cancel scheduled.
  firstChargeOn: string | null
  canceledAtTrialEnd: boolean
  // Canceled before its trial ended (an immediate cancel mid-trial).
  canceledDuringTrial: boolean
  next: NextEvent | null
}

// A cancel at trial end stamps canceled_at at the trial end; allow a minute
// of skew between the two writes.
const SAME_INSTANT_MS = 60_000

function ms(iso?: string | null): number {
  return iso ? new Date(iso).getTime() : NaN
}

// periodLabels. `firstChargeTiming` is the first item's plan bill timing: an
// in_advance plan charges at trial end, an in_arrears plan at the first
// period's close (next_billing_at).
export function periodLabels(
  sub: SubscriptionPeriodInput,
  fmt: PeriodDateFormat,
  firstChargeTiming?: string,
): PeriodLabels {
  const live = sub.status === 'active' || sub.status === 'trialing'
  const trialing = sub.status === 'trialing'
  const canceled = sub.status === 'canceled'
  const scheduled = live && hasScheduledCancel(sub)

  const canceledMs = ms(sub.canceled_at)
  const trialEndMs = ms(sub.trial_end_at)
  const canceledAtTrialEnd = canceled && !Number.isNaN(canceledMs) && !Number.isNaN(trialEndMs) &&
    Math.abs(canceledMs - trialEndMs) <= SAME_INSTANT_MS
  const canceledDuringTrial = canceled && !Number.isNaN(canceledMs) && !Number.isNaN(trialEndMs) &&
    canceledMs < trialEndMs - SAME_INSTANT_MS

  let accessThrough: string | null = null
  if (trialing && sub.trial_end_at) accessThrough = fmt.lastDay(sub.trial_end_at)
  else if (live && sub.current_billing_period_end) accessThrough = fmt.lastDay(sub.current_billing_period_end)

  // cancel_effective_at is the server's answer (ADR-069): on a trialing
  // subscription the flag cancels at trial end, not at the first paid
  // period's end.
  const cancelAt = scheduled ? (sub.cancel_effective_at || sub.cancel_at || sub.current_billing_period_end) : undefined
  const cancelsOn = cancelAt ? fmt.date(cancelAt) : null

  const renewAt = live && !scheduled && !trialing ? (sub.next_billing_at || sub.current_billing_period_end) : undefined
  const renewsOn = renewAt ? fmt.date(renewAt) : null

  const trialEndsOn = sub.trial_end_at && (trialing || canceledAtTrialEnd || canceledDuringTrial) ? fmt.date(sub.trial_end_at) : null

  let firstChargeOn: string | null = null
  if (trialing && !scheduled) {
    const at = firstChargeTiming === 'in_advance' ? sub.trial_end_at : sub.next_billing_at
    if (at) firstChargeOn = fmt.date(at)
  }

  const canceledOn = canceled && sub.canceled_at ? fmt.date(sub.canceled_at) : null

  let next: NextEvent | null = null
  if (canceled) {
    if (canceledOn) {
      next = {
        label: canceledAtTrialEnd ? 'Canceled at trial end' : canceledDuringTrial ? 'Canceled during trial' : 'Canceled',
        value: canceledOn,
        tone: 'ended',
      }
    }
  } else if (cancelsOn) {
    next = { label: trialing ? 'Cancels at trial end' : 'Cancels', value: cancelsOn, tone: 'scheduled' }
  } else if (firstChargeOn) {
    next = { label: 'First charge', value: firstChargeOn, tone: 'normal' }
  } else if (trialing && trialEndsOn) {
    next = { label: 'Trial ends', value: trialEndsOn, tone: 'normal' }
  } else if (renewsOn) {
    next = { label: 'Renews', value: renewsOn, tone: 'normal' }
  }

  return { accessThrough, renewsOn, cancelsOn, canceledOn, trialEndsOn, firstChargeOn, canceledAtTrialEnd, canceledDuringTrial, next }
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

// A trial's length in whole days ("14-day trial"). Trial bounds sit on
// midnights in the billing timezone; rounding absorbs a DST hour.
export function trialLength(startISO: string, endISO: string): number {
  return Math.max(0, Math.round((new Date(endISO).getTime() - new Date(startISO).getTime()) / 86_400_000))
}
