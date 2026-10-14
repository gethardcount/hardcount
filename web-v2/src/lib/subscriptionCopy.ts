// Dialog and toast words on the subscription page that depend on state.
// Dates come in already formatted, so this stays plain data for node --test.

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
// server saved, if any.
export function pauseCollectionToast(resumesLabel: string | undefined): ToastCopy {
  return resumesLabel
    ? { title: 'Collection paused', description: `New invoices stay as drafts until ${resumesLabel}.` }
    : { title: 'Collection paused', description: 'New invoices stay as drafts until you resume collection.' }
}

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
