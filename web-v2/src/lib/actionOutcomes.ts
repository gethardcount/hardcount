// Which message an operator sees after an action, decided from what actually
// happened — never from what was requested. Pure, so each branch is pinned by
// tests/actionOutcomes.test.ts; the pages only render the result.

import { errorSentence, outcomeUnknown } from './errorCopy'

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

// What Issue credit note says when its request fails, and whether the
// invoice's Idempotency-Key (lib/retryKeys) is kept for the next try. A note
// can start a card refund. So the dialog must tell a try that may have made
// the note from one the server refused, and must keep the key while a try
// under it may have made one. The next try then sends the key again, and the
// server returns that note instead of making a second one.
//
// The answers, in the order they are read:
//   - No answer from the API: status 0, or an error that is not the API's.
//     The request may have been sent. Unknown; keep the key.
//   - An answer the server stored under the key and sent again
//     (Idempotent-Replayed). The try that got it first has ended, so settle.
//     A bare 5xx (lib/errorCopy outcomeUnknown) did not say whether it made
//     the note: the amounts read again now count it if it did
//     (ISSUE_CREDIT_ENDED). Any other stored answer says what happened.
//   - conflict_idempotency_unresolved: the try under the key died without an
//     answer. It has ended too (ISSUE_CREDIT_ENDED).
//   - A bare 5xx, or conflict_idempotency (a try under the key is still
//     running). Unknown; keep the key.
//   - idempotency_error: other values under a key the server has seen.
//     Nothing was made; settle.
//   - A refusal from the handler (HANDLER_REFUSALS), or a 5xx with its own
//     code such as provider_error: settle. The handler runs only for a key
//     with no stored answer, so no earlier try under the key made a note.
//   - Anything else: an answer given before the server reads the key. That
//     is the session check (401), the cross-site check (403), the rate limit
//     (429), or a service that is unavailable (503, such as
//     idempotency_unavailable when the key could not be reserved). It says
//     nothing about an earlier try under the key, so the key is kept. After
//     an earlier try whose result is unknown, it still reads as unknown.
export const ISSUE_CREDIT_UNKNOWN =
  "The credit note may have been made. The amounts above are read again, so they count it if it was. Issuing again with the same values can't make a second note, and says what became of the first try once it has ended."

// The server refuses other values under a key it has seen (422
// idempotency_error). After a try whose result is unknown, that means the
// operator changed the note.
export const ISSUE_CREDIT_VALUES_DIFFER =
  'Nothing was issued. These values differ from the try whose result is unknown. Check the amounts above: issuing again makes a new note.'

export const ISSUE_CREDIT_ENDED =
  "The earlier try with these values ended without saying whether it made the note. The amounts above are read again, so they count the note if it was made. If they don't count it, issuing again makes a new note."

export interface IssueCreditFailure {
  // true: the note may have been made, by this try or an earlier one under
  // the same key. Drawn as uncertain, not as an error.
  unknown: boolean
  // true: the answer says what became of the tries under the key. The key
  // is dropped, and the next try is a new note.
  settles: boolean
  message: string
}

// The statuses the credit-note handler refuses a request with. A 409 or 422
// from the idempotency layer has its own code, read before these.
const HANDLER_REFUSALS = new Set([400, 404, 409, 412, 422])

// earlierUnknown: an earlier try under the key may have made the note
// (RetryKeys.unknown).
export function issueCreditFailure(err: unknown, earlierUnknown: boolean): IssueCreditFailure {
  const { status, code, replayed } = typeof err === 'object' && err !== null
    ? (err as { status?: unknown; code?: unknown; replayed?: unknown })
    : {}
  const mayHaveMadeIt = { unknown: true, settles: false, message: ISSUE_CREDIT_UNKNOWN }
  if (typeof status !== 'number' || status === 0) return mayHaveMadeIt
  if (code === 'conflict_idempotency_unresolved' || (replayed === true && outcomeUnknown(err))) {
    return { unknown: true, settles: true, message: ISSUE_CREDIT_ENDED }
  }
  if (replayed === true) return { unknown: false, settles: true, message: errorSentence(err) }
  if (outcomeUnknown(err) || code === 'conflict_idempotency') return mayHaveMadeIt
  if (code === 'idempotency_error') return { unknown: false, settles: true, message: ISSUE_CREDIT_VALUES_DIFFER }
  if (HANDLER_REFUSALS.has(status) || (status >= 500 && status !== 503)) {
    return { unknown: false, settles: true, message: errorSentence(err) }
  }
  return earlierUnknown ? mayHaveMadeIt : { unknown: false, settles: false, message: errorSentence(err) }
}
