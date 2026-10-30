// Which message an operator sees after an action, decided from what actually
// happened — never from what was requested. Pure, so each branch is pinned by
// tests/actionOutcomes.test.ts; the pages only render the result.

export type OutcomeTone = 'success' | 'info' | 'warning' | 'error'

export interface Outcome {
  tone: OutcomeTone
  title: string
  description?: string
}

// collectOutcome reads the invoice that "Charge now" returns. A 200 is not
// "payment initiated": credit can settle the invoice with no charge at all,
// Stripe can confirm a card charge inside the request, a charge can still be
// confirming, and an outcome recorded by a webhook during the call comes back
// as-is (it can be a failure). Declines arrive as API errors, not here.
export function collectOutcome(inv: {
  invoice_number: string
  status: string
  payment_status: string
  amount_paid_cents: number
  stripe_payment_intent_id?: string | null
}): Outcome {
  const n = inv.invoice_number
  if (inv.status === 'paid' || inv.payment_status === 'succeeded') {
    // Only a credit-covered settle records no payment at all: it pays with no
    // PaymentIntent and leaves amount_paid at zero. amount_paid alone is not
    // enough — when Stripe confirms a card charge but re-reading the invoice
    // fails, the API returns the pre-charge copy (amount_paid still zero) with
    // the PaymentIntent set (internal/payment/stripe.go chargeInvoice).
    return inv.amount_paid_cents === 0 && !inv.stripe_payment_intent_id
      ? { tone: 'success', title: `${n} paid`, description: "Covered by the customer's credit balance. No card was charged." }
      : { tone: 'success', title: `${n} paid` }
  }
  switch (inv.payment_status) {
    case 'processing':
      return { tone: 'info', title: 'Payment started', description: 'Stripe is confirming it. This page updates when it is done.' }
    case 'failed':
      return { tone: 'error', title: `${n} payment failed`, description: 'The reason is shown on this invoice.' }
    case 'unknown':
      return { tone: 'warning', title: 'Payment status unknown', description: "Hardcount couldn't confirm the result with Stripe. See the notice on this invoice." }
    default:
      return { tone: 'info', title: `${n} not charged`, description: 'No payment was made. Refresh to see the latest status.' }
  }
}

// writeOffOutcome: marking an invoice uncollectible and pausing its
// subscription are two requests. The write-off has already happened when the
// pause runs, so a failed pause must not be reported as either "failed" (the
// write-off stands) or "collection paused" (it is not) — one warning says both.
export function writeOffOutcome(
  invoiceNumber: string,
  pause: 'not_requested' | 'paused' | 'failed',
): Outcome & { openSubscription: boolean } {
  const n = invoiceNumber
  switch (pause) {
    case 'failed':
      return {
        tone: 'warning',
        title: 'Collection not paused',
        description: `${n} was written off, but the subscription is still billing. Pause it from the subscription page.`,
        openSubscription: true,
      }
    case 'paused':
      return { tone: 'success', title: `${n} marked uncollectible, collection paused`, openSubscription: false }
    default:
      return { tone: 'success', title: `${n} marked uncollectible`, openSubscription: false }
  }
}

// newInvoiceOutcome: the one message after the one-off invoice composer.
// Finalizing and emailing are separate steps; when the email step does not
// happen, the invoice is still finalized (not a draft), and that is said once.
export type NewInvoiceResult =
  | { kind: 'draft' }
  | { kind: 'sent' }
  | { kind: 'email_failed'; recipient: string }
  | { kind: 'no_email' }
  | { kind: 'email_not_configured' }

// emailStepAfterFinalize: what Save & send still has to do once the invoice
// is finalized. Without Stripe, finalizing a card-less customer's invoice
// already emails them the invoice (invoice_emailed_at, ADR-060 amendment
// 2026-10-30), and sending it again would email it twice. Otherwise it is
// sent to the customer's address, or there is none to send to.
export function emailStepAfterFinalize(
  finalized: { invoice_emailed_at?: string },
  recipient: string | undefined,
): 'already_emailed' | 'send' | 'no_email' {
  if (finalized.invoice_emailed_at) return 'already_emailed'
  return recipient ? 'send' : 'no_email'
}

export function newInvoiceOutcome(invoiceNumber: string, result: NewInvoiceResult): Outcome {
  switch (result.kind) {
    case 'sent':
      return { tone: 'success', title: `Invoice ${invoiceNumber} sent` }
    case 'email_failed':
      return {
        tone: 'warning',
        title: `Invoice ${invoiceNumber} finalized, email not sent`,
        description: `Couldn't email ${result.recipient}. Send it from the invoice page.`,
      }
    case 'email_not_configured':
      return {
        tone: 'warning',
        title: `Invoice ${invoiceNumber} finalized, email not sent`,
        description: EMAIL_NOT_CONFIGURED_MESSAGE,
      }
    case 'no_email':
      return {
        tone: 'warning',
        title: `Invoice ${invoiceNumber} finalized, not emailed`,
        description: 'The customer has no email address. Add one, then send it from the invoice page.',
      }
    default:
      return { tone: 'success', title: `Draft ${invoiceNumber} saved` }
  }
}

// Every operator email action (send invoice, resend or send a setup link, send
// a credit note, invite a teammate) answers 409 with this code when the server
// has no SMTP. Emails are queued, so before the refusal existed the action said
// "sent" and the queued email then failed out of sight. Error toasts show the
// server's sentence for it (lib/errorCopy, a 4xx refusal: no Copy ID); Save &
// send reads the code to report a finalized invoice that was not emailed.
export const EMAIL_NOT_CONFIGURED = 'email_not_configured'
export const EMAIL_NOT_CONFIGURED_MESSAGE = "Email isn't set up on this server, so nothing was sent."

export function isEmailNotConfigured(err: unknown): boolean {
  return typeof err === 'object' && err !== null && (err as { code?: unknown }).code === EMAIL_NOT_CONFIGURED
}

// marginCardView picks what the Margin card body shows. Without the error
// branch a failed load kept the spinner forever; without the window branch, a
// custom range with one date missing did the same (the query never runs).
export type MarginCardView = 'pick-dates' | 'error' | 'loading' | 'ready'

export function marginCardView(s: { hasWindow: boolean; isError: boolean; hasData: boolean }): MarginCardView {
  if (!s.hasWindow) return 'pick-dates'
  if (s.isError) return 'error'
  if (s.hasData) return 'ready'
  return 'loading'
}

// recordStripeRefundOutcome reads the credit note that "Record as credit
// note" returns (ADR-134). Its card refund is the refund already made in
// Stripe, so the message says no second refund went out.
export function recordStripeRefundOutcome(cn: { credit_note_number: string; status: string }): Outcome {
  if (cn.status !== 'issued') {
    return { tone: 'warning', title: `${cn.credit_note_number} not issued yet`, description: 'Refresh to check it before trying again.' }
  }
  return {
    tone: 'success',
    title: `Recorded as ${cn.credit_note_number}`,
    description: 'The refund made in Stripe is on this invoice now. Nothing was refunded again.',
  }
}

// attachStripeRefundOutcome reads the credit note that "Attach to …" returns:
// the note whose own refund failed now has the refund made in Stripe as its
// card refund.
export function attachStripeRefundOutcome(cn: { credit_note_number: string; refund_status: string }): Outcome {
  if (cn.refund_status !== 'succeeded') {
    return { tone: 'warning', title: `${cn.credit_note_number} not updated yet`, description: 'Refresh to check it before trying again.' }
  }
  return {
    tone: 'success',
    title: `Refund attached to ${cn.credit_note_number}`,
    description: 'Its card refund is now the refund made in Stripe. Nothing was refunded again.',
  }
}

// Retry refund's refusal while the invoice has a refund made in Stripe that
// is not recorded yet (409, ADR-134): a retry could refund the customer
// twice. The server's sentence says what to do; the toast adds a way to the
// invoice, where the buttons are.
export const STRIPE_REFUND_UNRESOLVED = 'stripe_refund_unresolved'

export function isStripeRefundUnresolved(err: unknown): boolean {
  return typeof err === 'object' && err !== null && (err as { code?: unknown }).code === STRIPE_REFUND_UNRESOLVED
}
