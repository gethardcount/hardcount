// The invoice page shows at most one notice above the invoice, and the
// written-off notice says what the subscription is really doing (it used to
// say "The subscription remains active" even when dunning had paused it).
import { test } from 'vitest'
import assert from 'node:assert/strict'
import {
  finalizeConfirmLines,
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

test('written-off line: a canceled subscription says canceled, even if a pause is still recorded', () => {
  for (const status of ['canceled']) {
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

// #1109: Finalize asks first, and says what it will do for this customer.
test('the finalize confirmation says what finalizing does', () => {
  const card = finalizeConfirmLines({ hasCard: true, hasEmail: true, canEmailSetupLinks: true, taxAtFinalize: false })
  assert.match(card[0], /can't edit it afterwards/)
  assert.match(card[0], /void/)
  assert.equal(card.length, 2)
  assert.match(card[1], /credit balance first/)
  assert.match(card[1], /charges their card on file/)
  assert.doesNotMatch(card[1], /emails/)

  const noCard = finalizeConfirmLines({ hasCard: false, hasEmail: true, canEmailSetupLinks: true, taxAtFinalize: false })
  assert.match(noCard[1], /no card on file/)
  assert.match(noCard[1], /emails them a link/)
  assert.doesNotMatch(noCard[1], /charges/)

  const nowhere = finalizeConfirmLines({ hasCard: false, hasEmail: false, canEmailSetupLinks: true, taxAtFinalize: false })
  assert.match(nowhere[1], /nothing is charged/)
  assert.doesNotMatch(nowhere[1], /emails/)

  const unknown = finalizeConfirmLines({ hasCard: undefined, hasEmail: true, canEmailSetupLinks: true, taxAtFinalize: false })
  assert.match(unknown[1], /charges their card on file, or emails them a link/)

  // A manual invoice gets its tax at finalize.
  const manual = finalizeConfirmLines({ hasCard: true, hasEmail: true, canEmailSetupLinks: true, taxAtFinalize: true })
  assert.equal(manual.length, 3)
  assert.match(manual[1], /Tax is worked out now/)
})

// A server without SMTP or PAYMENT_UPDATE_URL sends no setup-link email, so
// the confirmation must not promise one (whoami.can_email_setup_links).
test('the finalize confirmation does not promise an email the server cannot send', () => {
  for (const canEmailSetupLinks of [false, undefined]) {
    const noCard = finalizeConfirmLines({ hasCard: false, hasEmail: true, canEmailSetupLinks, taxAtFinalize: false })
    assert.doesNotMatch(noCard[1], /emails/)
    assert.match(noCard[1], /can't email setup links/)
    assert.match(noCard[1], /customer page/)

    const unknown = finalizeConfirmLines({ hasCard: undefined, hasEmail: true, canEmailSetupLinks, taxAtFinalize: false })
    assert.doesNotMatch(unknown[1], /emails/)
    assert.match(unknown[1], /charges their card on file if they have one/)
  }
})
