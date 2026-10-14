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
      return { tone: 'warning', title: 'Payment status unknown', description: "Velox couldn't confirm the result with Stripe. See the notice on this invoice." }
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
// "sent" and the queued email then failed out of sight. Retrying cannot help,
// so the toast carries no request ID to copy.
export const EMAIL_NOT_CONFIGURED = 'email_not_configured'
export const EMAIL_NOT_CONFIGURED_MESSAGE = "Email isn't set up on this server, so nothing was sent."

export function isEmailNotConfigured(err: unknown): boolean {
  return typeof err === 'object' && err !== null && (err as { code?: unknown }).code === EMAIL_NOT_CONFIGURED
}

export function emailNotConfiguredOutcome(): Outcome {
  return { tone: 'error', title: 'Email not sent', description: EMAIL_NOT_CONFIGURED_MESSAGE }
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
