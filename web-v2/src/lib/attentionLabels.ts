// Plain labels for the server's attention reason codes. The server sends a
// typed code; the dashboard owns the words, so wording changes need no
// server release. Shared by the invoice page notice and the invoice list,
// so the two never disagree. A raw code ("payment_anomaly") is never shown:
// an unmapped reason falls back to a generic label.

const REASON_LABELS: Record<string, string> = {
  tax_calculation_failed: 'Tax calculation failed',
  tax_location_required: 'Customer address required',
  payment_failed: 'Payment failed',
  payment_unconfirmed: 'Payment unconfirmed',
  payment_processing: 'Payment processing',
  payment_scheduled: 'Auto-charge scheduled',
  awaiting_payment: 'Awaiting first charge',
  no_payment_method: 'No payment method',
  dunning_exhausted: 'Payment recovery ended',
  collection_paused: 'Collection paused',
  // A double charge, an amount or currency mismatch, or a payment that
  // arrived on a voided invoice: someone must look at it.
  payment_anomaly: 'Payment needs review',
  // Operator language, not ledger language. "Commit exposure" is the term the
  // ADR and the schema use; a finance operator reading a chip needs the plain
  // fact — an invoice went unpaid and the credit it bought is already live.
  commit_exposure: 'Unpaid prepaid credit',
}

export function attentionReasonLabel(reason: string, code?: string): string {
  // A PARKED invoice shares the payment_unconfirmed REASON with a charge that is
  // genuinely still being confirmed, but they are opposite situations — one
  // resolves itself, the other never will. The server distinguishes them by
  // CODE, so the label does too.
  if (code === 'payment.unidentifiable') return 'Payment unidentifiable'
  return REASON_LABELS[reason] ?? 'Needs attention'
}

// supportCode is the line that goes inside the notice's Details, so a
// screenshot sent to support still names the exact case.
export function supportCodeLine(code: string | undefined): string | null {
  return code ? `Support code: ${code}` : null
}
