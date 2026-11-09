// Pins which message an operator sees for which real outcome. Each of these
// was a message that claimed something the code had not done: "Payment
// initiated" for an invoice credit settled with no charge, "collection paused"
// after the pause failed, "Draft saved" for a finalized invoice, and a spinner
// that never stopped after a failed load.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import {
  attachStripeRefundOutcome,
  collectOutcome,
  emailStepAfterFinalize,
  isEmailNotConfigured,
  ISSUE_CREDIT_ENDED,
  ISSUE_CREDIT_UNKNOWN,
  ISSUE_CREDIT_VALUES_DIFFER,
  issueCreditFailure,
  isStripeRefundUnresolved,
  marginCardView,
  newInvoiceOutcome,
  recordStripeRefundOutcome,
  writeOffOutcome,
} from '../src/lib/actionOutcomes.ts'
import { errorSentence } from '../src/lib/errorCopy.ts'

const inv = (status: string, payment_status: string, amount_paid_cents = 0, stripe_payment_intent_id?: string) => ({
  invoice_number: 'INV-1', status, payment_status, amount_paid_cents, stripe_payment_intent_id,
})

test('charge now: a card charge Stripe confirmed in the request reads as paid', () => {
  const o = collectOutcome(inv('paid', 'succeeded', 1200, 'pi_1'))
  assert.equal(o.tone, 'success')
  assert.equal(o.title, 'INV-1 paid')
  assert.equal(o.description, undefined)
})

test('charge now: a credit-covered invoice says no card was charged', () => {
  const o = collectOutcome(inv('paid', 'succeeded', 0))
  assert.equal(o.title, 'INV-1 paid')
  assert.match(o.description ?? '', /credit balance/)
  assert.match(o.description ?? '', /No card was charged/)
})

test('charge now: a card charge returned before amount_paid was re-read never claims credit covered it', () => {
  // Stripe confirmed the charge, the re-read failed, and the API returned the
  // pre-charge copy: paid, amount_paid still 0, PaymentIntent set.
  const o = collectOutcome(inv('paid', 'succeeded', 0, 'pi_1'))
  assert.equal(o.tone, 'success')
  assert.equal(o.title, 'INV-1 paid')
  assert.equal(o.description, undefined)
})

test('charge now: a charge still confirming is "started", not paid', () => {
  const o = collectOutcome(inv('finalized', 'processing'))
  assert.equal(o.tone, 'info')
  assert.equal(o.title, 'Payment started')
})

test('charge now: a failure recorded during the call is an error', () => {
  const o = collectOutcome(inv('finalized', 'failed'))
  assert.equal(o.tone, 'error')
  assert.equal(o.title, 'INV-1 payment failed')
})

test('charge now: an unknown outcome is a warning', () => {
  const o = collectOutcome(inv('finalized', 'unknown'))
  assert.equal(o.tone, 'warning')
  assert.equal(o.title, 'Payment status unknown')
})

test('charge now: nothing ever reads "Payment initiated"', () => {
  for (const [s, p] of [['paid', 'succeeded'], ['finalized', 'processing'], ['finalized', 'failed'], ['finalized', 'unknown'], ['finalized', 'pending']]) {
    const o = collectOutcome(inv(s, p))
    assert.notEqual(o.title, 'Payment initiated')
    if (p !== 'succeeded') assert.notEqual(o.tone, 'success', `${p} must not read as success`)
  }
})

test('write-off: a failed pause is one warning that says collection was NOT paused', () => {
  const o = writeOffOutcome('INV-1', 'failed')
  assert.equal(o.tone, 'warning')
  assert.equal(o.title, 'Collection not paused')
  assert.equal(o.description, 'INV-1 was written off, but the subscription is still billing. Pause it from the subscription page.')
  assert.equal(o.openSubscription, true)
})

test('write-off: "collection paused" appears only when the pause succeeded', () => {
  assert.equal(writeOffOutcome('INV-1', 'paused').title, 'INV-1 marked uncollectible, collection paused')
  assert.equal(writeOffOutcome('INV-1', 'paused').tone, 'success')
  assert.doesNotMatch(writeOffOutcome('INV-1', 'not_requested').title, /paused/)
  assert.doesNotMatch(writeOffOutcome('INV-1', 'failed').title, /collection paused/)
})

test('write-off: every title fits the six-word notice shape', () => {
  for (const p of ['not_requested', 'paused', 'failed'] as const) {
    const words = writeOffOutcome('INV-1', p).title.split(/\s+/).length
    assert.ok(words <= 6, `${p}: ${words} words`)
  }
})

test('new invoice: each result gets its own single message', () => {
  assert.equal(newInvoiceOutcome('INV-9', { kind: 'draft' }).title, 'Draft INV-9 saved')
  assert.equal(newInvoiceOutcome('INV-9', { kind: 'sent' }).title, 'Invoice INV-9 sent')

  const failed = newInvoiceOutcome('INV-9', { kind: 'email_failed', recipient: 'a@b.co' })
  assert.equal(failed.tone, 'warning')
  assert.equal(failed.title, 'Invoice INV-9 finalized, email not sent')
  assert.match(failed.description ?? '', /a@b\.co/)

  const noEmail = newInvoiceOutcome('INV-9', { kind: 'no_email' })
  assert.equal(noEmail.tone, 'warning')
  assert.match(noEmail.description ?? '', /no email address/)
})

test('new invoice: Save & send on a server without email says the invoice was finalized and not emailed', () => {
  const o = newInvoiceOutcome('INV-9', { kind: 'email_not_configured' })
  assert.equal(o.tone, 'warning')
  assert.equal(o.title, 'Invoice INV-9 finalized, email not sent')
  assert.equal(o.description, "Email isn't set up on this server, so nothing was sent.")
  assert.doesNotMatch(o.description ?? '', /Send it from the invoice page/)
})

test('email not configured: recognised by its code', () => {
  assert.equal(isEmailNotConfigured({ code: 'email_not_configured', status: 409 }), true)
  assert.equal(isEmailNotConfigured({ code: 'no_email_on_file' }), false)
  assert.equal(isEmailNotConfigured(new Error('x')), false)
  assert.equal(isEmailNotConfigured(null), false)
})

test('new invoice: a finalized invoice is never called a draft', () => {
  for (const r of [{ kind: 'email_failed', recipient: 'x@y.z' }, { kind: 'no_email' }, { kind: 'email_not_configured' }] as const) {
    assert.doesNotMatch(newInvoiceOutcome('INV-9', r).title, /Draft/)
  }
})

test('margin card: an error stops the spinner', () => {
  assert.equal(marginCardView({ hasWindow: true, isError: true, hasData: false }), 'error')
})

test('margin card: a custom range missing a date asks for it instead of spinning', () => {
  assert.equal(marginCardView({ hasWindow: false, isError: false, hasData: false }), 'pick-dates')
})

test('margin card: loading only while a request can still answer', () => {
  assert.equal(marginCardView({ hasWindow: true, isError: false, hasData: false }), 'loading')
  assert.equal(marginCardView({ hasWindow: true, isError: false, hasData: true }), 'ready')
})

// Without Stripe, finalizing already emails a card-less customer the invoice
// (invoice_emailed_at on the finalize response), so Save & send must not send
// it again: the customer would get it twice.
test('Save & send: an invoice the finalize already emailed is not sent again', () => {
  assert.equal(emailStepAfterFinalize({ invoice_emailed_at: '2026-10-30T10:00:00Z' }, 'ap@acme.test'), 'already_emailed')
  assert.equal(emailStepAfterFinalize({}, 'ap@acme.test'), 'send')
  assert.equal(emailStepAfterFinalize({}, undefined), 'no_email')
  assert.equal(emailStepAfterFinalize({}, ''), 'no_email')
})

test('record a refund made in Stripe: an issued note says nothing was refunded again', () => {
  const o = recordStripeRefundOutcome({ credit_note_number: 'CN-0003', status: 'issued' })
  assert.equal(o.tone, 'success')
  assert.equal(o.title, 'Recorded as CN-0003')
  assert.match(o.description ?? '', /Nothing was refunded again/)
})

test('record a refund made in Stripe: a note that is not issued is not called recorded', () => {
  const o = recordStripeRefundOutcome({ credit_note_number: 'CN-0003', status: 'draft' })
  assert.equal(o.tone, 'warning')
  assert.doesNotMatch(o.title, /Recorded/)
})

test('attach a refund made in Stripe: success only when the note now shows the refund succeeded', () => {
  assert.equal(attachStripeRefundOutcome({ credit_note_number: 'CN-0001', refund_status: 'succeeded' }).title, 'Refund attached to CN-0001')
  assert.equal(attachStripeRefundOutcome({ credit_note_number: 'CN-0001', refund_status: 'failed' }).tone, 'warning')
})

test('Retry refund refused over an unrecorded refund made in Stripe is told apart by its code', () => {
  assert.equal(isStripeRefundUnresolved({ code: 'stripe_refund_unresolved' }), true)
  assert.equal(isStripeRefundUnresolved({ code: 'invalid_state' }), false)
  assert.equal(isStripeRefundUnresolved(null), false)
})

// An API error as lib/api throws it, for the request Issue credit note sends.
// replayed: the answer is the stored answer of an earlier try under the same
// key (the server's Idempotent-Replayed header).
function apiError(message: string, status: number, code?: string, replayed?: boolean): Error {
  return Object.assign(new Error(message), { status, code, method: 'POST', replayed })
}

// Issue credit note can start a card refund. A try whose answer does not
// settle it may have made the note, so the dialog says the result is
// unknown and the next try sends the same Idempotency-Key: the server then
// returns that note instead of making a second one.
test('Issue credit note: a try that may have made the note reads as unknown, so the next try keeps its key', () => {
  const unknown: [string, unknown][] = [
    ['no answer', apiError('Failed to fetch', 0, 'network')],
    ['no answer in 40 seconds', apiError('No response from the server within 40 seconds.', 0, 'timeout')],
    ['a proxy 502 with no body', apiError('Bad Gateway', 502)],
    ['a server fault', apiError('an internal error occurred', 500, 'internal_error')],
    ['the same key still running', apiError('A request with this idempotency key is still being processed. Please retry.', 409, 'conflict_idempotency')],
    ['an error from the page, not the API', new TypeError('network error')],
  ]
  for (const [name, err] of unknown) {
    for (const earlierUnknown of [false, true]) {
      assert.deepEqual(issueCreditFailure(err, earlierUnknown), { unknown: true, settles: false, message: ISSUE_CREDIT_UNKNOWN }, name)
    }
  }
  // It names the note, never the raw transport text.
  assert.doesNotMatch(ISSUE_CREDIT_UNKNOWN, /fetch|40 seconds|idempotency/i)
})

// The server gives these answers before it reads the key: the session check
// (401), the cross-site check (403), the rate limit (429), and a service
// that is unavailable (503), such as a failed reservation of the key. They
// say nothing about an earlier try under the key, so the key is kept.
// Dropping it would let the next try make a second note.
test('Issue credit note: a refusal before the server reads the key keeps the key, and says unknown only after an unknown try', () => {
  const beforeTheKey: [string, Error][] = [
    ['the session check', apiError('missing credentials — sign in at /login or send Authorization: Bearer hc_secret_...', 401, 'unauthorized')],
    ['the cross-site check', apiError('cross-site request blocked', 403, 'forbidden')],
    ['the rate limit', apiError('too many requests — please retry after the rate limit resets', 429, 'rate_limited')],
    ['a failed reservation of the key', apiError('could not record the idempotency key — retry the request with the same key', 503, 'idempotency_unavailable')],
    ['the sign-in store', apiError('authentication is temporarily unavailable — retry shortly', 503, 'authentication_unavailable')],
  ]
  for (const [name, err] of beforeTheKey) {
    assert.deepEqual(issueCreditFailure(err, true), { unknown: true, settles: false, message: ISSUE_CREDIT_UNKNOWN }, `${name}, after an unknown try`)
    assert.deepEqual(issueCreditFailure(err, false), { unknown: false, settles: false, message: errorSentence(err) }, `${name}, on a first try`)
  }
})

test('Issue credit note: a refusal is a known failure in the server sentence, and the next try is a new note', () => {
  const refusals: [Error, string][] = [
    [apiError('credit note total exceeds what is still creditable on this invoice', 409, 'invalid_state'), 'Credit note total exceeds what is still creditable on this invoice.'],
    [apiError('reason is required', 422, 'validation_error'), 'Reason is required.'],
    [apiError('unknown field "refund"', 400, 'invalid_request'), 'Unknown field "refund".'],
    [apiError('invoice was changed', 412, 'precondition_failed'), 'Invoice was changed.'],
    [apiError('invoice could not be found', 404, 'invoice_not_found'), 'Invoice could not be found.'],
    [apiError('Stripe refused the refund: charge already refunded', 502, 'provider_error'), 'Stripe refused the refund: charge already refunded.'],
  ]
  for (const [err, message] of refusals) {
    for (const earlierUnknown of [false, true]) {
      assert.deepEqual(issueCreditFailure(err, earlierUnknown), { unknown: false, settles: true, message }, message)
    }
  }
})

test('Issue credit note: other values under a kept key are refused, and the sentence says why in plain words', () => {
  const err = apiError('Keys for idempotent requests can only be used with the same parameters they were first used with.', 422, 'idempotency_error')
  assert.deepEqual(issueCreditFailure(err, true), { unknown: false, settles: true, message: ISSUE_CREDIT_VALUES_DIFFER })
  assert.doesNotMatch(ISSUE_CREDIT_VALUES_DIFFER, /idempotent|parameters/i)
})

// The server stores a 5xx under the key, and answers every later try with
// that key with it, marked Idempotent-Replayed. The try that got it first
// has ended, so the amounts read again are final: they count its note if it
// made one. Keeping the key would only replay the 5xx for 24 hours. A try
// the server never recorded an answer for (it died during the try) has
// ended too.
test('Issue credit note: a try that ended without saying whether it made the note settles the key, and says to check the amounts', () => {
  const ended: [string, Error][] = [
    ['a stored server fault', apiError('an internal error occurred', 500, 'internal_error', true)],
    ['a stored timeout', apiError('the request ran past its time limit', 504, 'timeout', true)],
    ['a try the server never finished', apiError('A request with this idempotency key started 6m ago and never recorded an outcome', 409, 'conflict_idempotency_unresolved')],
  ]
  for (const [name, err] of ended) {
    assert.deepEqual(issueCreditFailure(err, true), { unknown: true, settles: true, message: ISSUE_CREDIT_ENDED }, name)
  }
  // A stored refusal says what happened, in the server's sentence.
  assert.deepEqual(
    issueCreditFailure(apiError('Stripe refused the refund: charge already refunded', 502, 'provider_error', true), true),
    { unknown: false, settles: true, message: 'Stripe refused the refund: charge already refunded.' },
  )
  assert.doesNotMatch(ISSUE_CREDIT_ENDED, /idempotency|replay|5\d\d/i)
})
