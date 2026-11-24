// creditNoteReasonLabel humanizes the credit-note `reason` for display.
// System-generated credit notes carry machine-categorical codes
// (subscription_downgrade, subscription_item_removed, …); operator-issued
// ones carry free text. Map the known codes to finance-readable phrases;
// pass anything else through unchanged (it's operator free text).
export function creditNoteReasonLabel(reason: string): string {
  switch (reason) {
    case 'subscription_downgrade': return 'Plan downgrade'
    case 'subscription_quantity_decrease': return 'Quantity decrease'
    case 'subscription_item_removed': return 'Item removed'
    case 'subscription_cancellation': return 'Cancellation'
    default: return reason
  }
}

// paymentIsUnresolved mirrors domain.InvoicePaymentStatus.IsInFlight(), the
// server's single source for whether a payment blocks operator actions
// (domain.PaymentBlocksAction). Call it instead of comparing payment_status by
// hand: `!== 'succeeded' && !== 'processing'` misses 'unknown' (Stripe has not
// confirmed the charge yet), and offers Collect payment on an invoice that may
// already be charged.
//
// This is the one copy. The durable version is the server returning
// per-action availability so the dashboard derives instead of mirroring;
// until then, this is the single place to change.
export function paymentIsUnresolved(paymentStatus: string): boolean {
  return paymentStatus === 'processing' || paymentStatus === 'unknown'
}

// paymentStatusLabel names an invoice's payment_status in words, for the
// invoice list's filter chip ("payment: unknown" showed the raw value). An
// unrecognised value is shown as itself rather than hidden.
export function paymentStatusLabel(status: string): string {
  switch (status) {
    case 'pending': return 'Payment pending'
    case 'processing': return 'Payment processing'
    case 'succeeded': return 'Paid'
    case 'failed': return 'Payment failed'
    // Stripe has not confirmed the charge yet.
    case 'unknown': return 'Payment status unknown'
    default: return `Payment ${status.replace(/_/g, ' ')}`
  }
}
