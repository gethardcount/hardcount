// Dialog and toast words on the subscription page that depend on state.
// Dates come in already formatted, so this stays plain data for the unit tests.

export interface ToastCopy {
  title: string
  description?: string
}

// After "Schedule cancellation". The server's cancel_effective_at is the
// answer (ADR-069): on a trialing subscription the flag cancels at trial
// end, and the customer is never charged.
export function scheduleCancelToast(
  updated: { status?: string } | undefined,
  effectiveLabel: string | undefined,
): ToastCopy {
  if (updated?.status === 'trialing' && effectiveLabel) {
    return { title: 'Cancellation scheduled', description: `Cancels at trial end, ${effectiveLabel}. The customer won't be charged.` }
  }
  if (effectiveLabel) {
    return { title: 'Cancellation scheduled', description: `Cancels at the end of the current period, ${effectiveLabel}.` }
  }
  return { title: 'Cancellation scheduled', description: 'Cancels at the end of the current period.' }
}

// After "Pause collection". resumesLabel is the formatted resumes_at the
// server saved, if any. "Saved as drafts until" bounds when NEW invoices
// are drafts; it must not say they get charged on resume: a period that
// ended while paused keeps its draft for good (subscription.Service
// ResumeCollection, billing.Engine reads the pause at the cycle close).
export function pauseCollectionToast(resumesLabel: string | undefined): ToastCopy {
  return resumesLabel
    ? { title: 'Collection paused', description: `New invoices are saved as drafts until ${resumesLabel}.` }
    : { title: 'Collection paused', description: 'New invoices are saved as drafts until you resume collection.' }
}

// The pause dialog sentence. Drafts made while paused are never charged by
// themselves; the operator finalizes one to collect it (finalize collects
// through the billing engine's collector).
export const PAUSE_DIALOG_SENTENCE =
  'New invoices are saved as drafts and not charged. Drafts made while paused stay drafts after you resume; finalize them to collect.'

// The "Immediately" choice and the typed confirm after it. Cancelling now
// bills a final invoice for time and usage so far (billing.Engine
// BillFinalOnImmediateCancel) and credits unused prepaid time; a trial
// that never turned paid bills nothing (trials are free, ADR-069). The old
// "Stops billing right now" hid that final charge.
export function cancelImmediatelyText(status: string): string {
  if (status === 'trialing') {
    return "Ends the trial and the subscription now. Nothing is billed. This can't be undone."
  }
  return "Ends the subscription now. Time and usage so far go on a final invoice; unused prepaid time is credited. This can't be undone."
}

// After an item's plan change with no proration to report: a scheduled
// change (pending_plan_id set) waits for the next period.
export function planChangeToast(item: { pending_plan_id?: string | null }, effectiveLabel: string | undefined): ToastCopy {
  if (item.pending_plan_id) {
    return {
      title: 'Plan change scheduled',
      description: effectiveLabel ? `The new plan starts ${effectiveLabel}.` : 'The new plan starts at the next billing period.',
    }
  }
  return { title: 'Plan changed' }
}

// Mid-period item changes. What happens to the money depends on the plan's
// bill timing (subscription.Handler item proration gates):
//   - in_advance: the difference for the rest of the period is invoiced or
//     credited at once. A net charge against an unpaid period invoice is
//     refused with its own message; a credit is taken off that invoice.
//   - in_arrears: nothing happens now; the period's invoice at its end
//     bills each part of the period at the plan and quantity in effect then.
// timing is the effective plan's base_bill_timing, or undefined when the
// plan isn't loaded, which gets a sentence true for both.
export type BillTiming = 'in_advance' | 'in_arrears' | string | undefined

export function changePlanNowHelper(timing: BillTiming): string {
  if (timing === 'in_advance') return 'The price difference for the rest of the period is charged or credited now.'
  if (timing === 'in_arrears') return 'The new price applies from now. The period is billed at its end, each part at the plan in effect then.'
  return 'The new price applies from now.'
}

export function changeQuantityText(timing: BillTiming): string {
  if (timing === 'in_advance') return 'Takes effect now. An increase is charged for the rest of the period; a decrease is credited.'
  if (timing === 'in_arrears') return 'Takes effect now. The period is billed at its end, each part at the quantity in effect then.'
  return 'Takes effect now.'
}

export function removeItemText(timing: BillTiming): string {
  if (timing === 'in_advance') return 'Removed mid-period, the unused part of the period is credited.'
  if (timing === 'in_arrears') return 'Removed mid-period, it is billed at the period end for the days it was on.'
  return ''
}
