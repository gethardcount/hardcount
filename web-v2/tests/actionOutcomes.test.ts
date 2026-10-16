// Pins which message an operator sees for which real outcome. Each of these
// was a message that claimed something the code had not done: "Payment
// initiated" for an invoice credit settled with no charge, "collection paused"
// after the pause failed, "Draft saved" for a finalized invoice, and a spinner
// that never stopped after a failed load.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import {
  collectOutcome,
  isEmailNotConfigured,
  marginCardView,
  newInvoiceOutcome,
  writeOffOutcome,
} from '../src/lib/actionOutcomes.ts'

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
