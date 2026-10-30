// Refunds made in Stripe outside Hardcount (ADR-134): what the invoice page
// offers for each one Hardcount flagged instead of recording. Pure, so each
// rule is pinned by tests/stripeRefunds.test.ts; the page only renders it.
//
// The buttons come from GET /v1/invoices/{id}/stripe-refunds, not from the
// notice's actions: the server sends the refunded_in_stripe notice with no
// actions, and on an invoice whose payment Hardcount also flagged, the notice
// keeps that anomaly's reason and button and only names the refund. Reading
// the refunds themselves covers both, and every refund the invoice holds.

// The fields of a refund made in Stripe these rules read
// (components["schemas"]["InvoiceStripeRefund"]).
export interface StripeRefundRow {
  stripe_refund_id: string
  amount_cents: number
  currency: string
  status: string
  reason?: string
  outcome: string
  review_reason?: string
}

// The fields of a credit note on the invoice these rules read.
export interface StripeRefundNote {
  id: string
  credit_note_number: string
  status: string
  refund_status: string
  refund_amount_cents: number
}

export interface StripeRefundChoices {
  // Credit notes whose own card refund failed and whose refund amount is this
  // refund's: the refund can become their card refund. Offered only for the
  // review reason credit_note_refund_failed, the case the server flags it for.
  attachTo: StripeRefundNote[]
  // Record it as a new credit note linked to the refund.
  record: boolean
  // Mark it handled outside Hardcount. Always offered for a flagged refund.
  dismiss: boolean
}

// Refunds the operator still has to act on, oldest first as the API sends
// them.
export function flaggedStripeRefunds<T extends StripeRefundRow>(refunds: readonly T[] | undefined): T[] {
  return (refunds ?? []).filter(r => r.outcome === 'needs_review')
}

// Review reasons whose refund can be recorded as a credit note. The others
// are refused by the server's checks, and the notice tells the operator to
// handle them in Stripe or their own books and dismiss them:
// exceeds_card_refundable and exceeds_creditable fail the caps,
// currency_mismatch fails the currency check, and prepaid_commit is retired
// with commit relief instead.
const RECORDABLE_REASONS = new Set(['seen_late', 'invoice_not_paid', 'credit_note_refund_failed'])

// stripeRefundChoices is what the notice offers for one flagged refund. Record
// and Attach follow the server's own checks, so a button is never offered for
// a request it refuses: the refund succeeded at Stripe, and Record needs a
// paid invoice (a refund flagged invoice_not_paid can be recorded once the
// invoice is paid).
export function stripeRefundChoices(
  refund: StripeRefundRow,
  invoice: { status: string; payment_status: string },
  creditNotes: readonly StripeRefundNote[],
): StripeRefundChoices {
  if (refund.outcome !== 'needs_review') return { attachTo: [], record: false, dismiss: false }
  const succeeded = refund.status === 'succeeded'
  const paid = invoice.status === 'paid' && invoice.payment_status === 'succeeded'
  const attachTo = succeeded && refund.review_reason === 'credit_note_refund_failed'
    ? creditNotes.filter(n =>
      n.status === 'issued' && n.refund_status === 'failed' && n.refund_amount_cents === refund.amount_cents)
    : []
  return {
    attachTo,
    record: succeeded && paid && RECORDABLE_REASONS.has(refund.review_reason ?? ''),
    dismiss: true,
  }
}

// The reason a recorded note carries, as the webhook path writes it
// (creditnote.stripeRefundCreditNoteReason), so a note recorded from the
// dashboard reads like one Hardcount recorded itself, and searching the
// credit notes for "Refunded in Stripe" finds both.
export function stripeRefundNoteReason(stripeReason: string | undefined): string {
  const r = (stripeReason ?? '').replace(/_/g, ' ').trim()
  return r ? `Refunded in Stripe: ${r}` : 'Refunded in Stripe'
}

// recordStripeRefundRequest is the POST /v1/credit-notes body that records a
// refund made in Stripe: one line for the refund's amount, all of it the card
// refund, which is that refund (stripe_refund_id), issued at once, nothing to
// the credit balance. The server reads the refund from Stripe and refuses any
// other amount. Same line as the webhook path writes.
export function recordStripeRefundRequest(invoiceId: string, refund: StripeRefundRow) {
  return {
    invoice_id: invoiceId,
    reason: stripeRefundNoteReason(refund.reason),
    refund_amount_cents: refund.amount_cents,
    credit_amount_cents: 0,
    out_of_band_amount_cents: 0,
    auto_issue: true,
    stripe_refund_id: refund.stripe_refund_id,
    lines: [{
      description: `Refund made in Stripe (${refund.stripe_refund_id})`,
      quantity: 1,
      unit_amount_cents: refund.amount_cents,
    }],
  }
}

// The most a dismissal note may be, as the server checks it.
const DISMISS_NOTE_MAX = 1000

// dismissNoteError is the line under the dismissal note, or '' when it can
// be sent. The server trims the note and counts characters (code points),
// not UTF-16 units, so an emoji counts once here too.
export function dismissNoteError(note: string): string {
  const n = note.trim()
  if (!n) return 'Say how you handled this refund.'
  if ([...n].length > DISMISS_NOTE_MAX) return `Keep it to ${DISMISS_NOTE_MAX} characters or fewer.`
  return ''
}
