// The invoice page shows at most one notice above the invoice, and the
// written-off notice says what the subscription is really doing (it used to
// say "The subscription remains active" even when dunning had paused it).
import { test } from 'vitest'
import assert from 'node:assert/strict'
import {
  messageLines,
  pickInvoiceNotice,
  stripePaymentUrl,
  writtenOffCardLine,
  writtenOffSubscriptionLine,
} from '../src/lib/invoiceNotice.ts'

const att = { severity: 'warning' }

test('a healthy invoice shows no notice', () => {
  assert.equal(pickInvoiceNotice({ status: 'finalized', attention: null }), null)
  assert.equal(pickInvoiceNotice({ status: 'paid' }), null)
})

test('an open invoice with attention shows the attention notice', () => {
  assert.equal(pickInvoiceNotice({ status: 'finalized', attention: att }), 'attention')
})

test('a written-off invoice without attention shows the written-off notice', () => {
  assert.equal(pickInvoiceNotice({ status: 'uncollectible', attention: null }), 'written_off')
  assert.equal(pickInvoiceNotice({ status: 'uncollectible' }), 'written_off')
})

test('on a written-off invoice, attention outranks the written-off notice (one notice, never two)', () => {
  // Anomaly, prepaid credit at risk, or a payment still being confirmed.
  for (const severity of ['critical', 'warning', 'info']) {
    assert.equal(pickInvoiceNotice({ status: 'uncollectible', attention: { severity } }), 'attention')
  }
})

test('written-off line: a subscription still billing offers Open subscription', () => {
  for (const status of ['active', 'trialing']) {
    const line = writtenOffSubscriptionLine({ status, pause_collection: null })
    assert.deepEqual(line, { state: 'billing', text: 'The subscription is still billing.', openSubscription: true })
  }
})

test('written-off line: paused collection says so, with no button', () => {
  const line = writtenOffSubscriptionLine({ status: 'active', pause_collection: { behavior: 'keep_as_draft' } })
  assert.deepEqual(line, { state: 'paused', text: 'Collection on the subscription is paused.', openSubscription: false })
})

test('written-off line: a canceled or archived subscription says canceled, even if a pause is still recorded', () => {
  for (const status of ['canceled', 'archived']) {
    const line = writtenOffSubscriptionLine({ status, pause_collection: { behavior: 'keep_as_draft' } })
    assert.equal(line?.state, 'ended')
    assert.equal(line?.text, 'The subscription is canceled.')
    assert.equal(line?.openSubscription, false)
  }
})

test('written-off line: a one-off invoice (no subscription) has no line', () => {
  assert.equal(writtenOffSubscriptionLine(undefined), null)
  assert.equal(writtenOffSubscriptionLine(null), null)
})

test('the never-true "remains active" wording is gone', () => {
  for (const sub of [
    { status: 'active' },
    { status: 'active', pause_collection: { behavior: 'keep_as_draft' } },
    { status: 'canceled' },
  ]) {
    assert.doesNotMatch(writtenOffSubscriptionLine(sub)?.text ?? '', /remains active/)
  }
})

test('message lines: one sentence, or a sentence and a second line', () => {
  assert.deepEqual(messageLines('One sentence.'), ['One sentence.'])
  assert.deepEqual(messageLines('First.\nSecond.'), ['First.', 'Second.'])
  assert.deepEqual(messageLines('First.\n\n'), ['First.'])
  assert.deepEqual(messageLines(undefined), [])
})

test('Open in Stripe goes to the payment, under /test/ in test mode', () => {
  assert.equal(stripePaymentUrl('pi_123', true), 'https://dashboard.stripe.com/payments/pi_123')
  assert.equal(stripePaymentUrl('pi_123', false), 'https://dashboard.stripe.com/test/payments/pi_123')
})

test('written off, no card: the email button needs an address AND a server that can send setup links', () => {
  assert.deepEqual(writtenOffCardLine('ap@acme.test', true), { action: 'email_link', note: null })
  // SMTP or PAYMENT_UPDATE_URL missing: the resend would fail, so no button.
  const off = writtenOffCardLine('ap@acme.test', false)
  assert.equal(off.action, 'open_customer')
  assert.match(off.note ?? '', /can't email setup links/)
  // Unknown (whoami still loading or failed) is not "yes".
  assert.equal(writtenOffCardLine('ap@acme.test', undefined).action, 'open_customer')
  // No address: the address is the first thing to fix, whatever the server can do.
  const noEmail = writtenOffCardLine('', true)
  assert.equal(noEmail.action, 'open_customer')
  assert.match(noEmail.note ?? '', /no email address/)
  assert.equal(writtenOffCardLine(null, false).note, noEmail.note)
})

test('messageLines keeps all three lines of a no-card commit invoice', () => {
  const msg = 'Card line.\nEmail line.\nUnpaid prepaid credit: amount line.'
  assert.deepEqual(messageLines(msg), ['Card line.', 'Email line.', 'Unpaid prepaid credit: amount line.'])
})
