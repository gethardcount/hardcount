// The Sell credit dialog: which ways to pay it offers and why one is not, the
// discount line, the validity line, and the confirm the top-up's page opens.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import {
  arrivalConfirm,
  confirmAfterCreate,
  creditValidityText,
  discountLine,
  payOptions,
  sellCreditAmounts,
  type PayFacts,
} from '../src/lib/sellCredit.ts'

const money = (c: number) => `$${(c / 100).toFixed(2)}`

const ready: PayFacts = {
  customerEmail: 'ap@acme.test', smtpConfigured: true, paymentPageSet: true, stripeConnected: true,
  livemode: false, card: { card_last4: '4242' },
}
const option = (f: PayFacts, value: string) => payOptions(f).find(o => o.value === value)!

test('every way to pay is offered when the customer and the server have what each needs', () => {
  assert.deepEqual(payOptions(ready).map(o => [o.label, o.state]), [
    ['Send them the payment link', 'available'],
    ['Charge the card ending 4242', 'available'],
    ['I already received the payment', 'available'],
    ['Decide later', 'available'],
  ])
})

test('the link says which of its four needs is missing, the first one only', () => {
  assert.equal(option({ ...ready, customerEmail: ' ' }, 'send_link').note, 'The customer has no email address.')
  assert.equal(option({ ...ready, smtpConfigured: false }, 'send_link').note, "Email isn't set up on this server.")
  assert.equal(option({ ...ready, paymentPageSet: false }, 'send_link').note, 'This server has no payment page address (HOSTED_INVOICE_BASE_URL).')
  assert.equal(option({ ...ready, stripeConnected: false, livemode: true }, 'send_link').note, "Stripe isn't connected in live mode.")
  assert.equal(option({ ...ready, customerEmail: '', smtpConfigured: false }, 'send_link').note, 'The customer has no email address.')
  for (const f of [{ customerEmail: '' }, { smtpConfigured: false }, { paymentPageSet: false }, { stripeConnected: false }]) {
    assert.equal(option({ ...ready, ...f }, 'send_link').state, 'unavailable')
  }
})

test('a charge needs a saved card and Stripe, and names the real card', () => {
  // What is missing in Settings links to where it is set up.
  assert.equal(option({ ...ready, stripeConnected: false }, 'send_link').href, '/settings?tab=payments')
  assert.equal(option({ ...ready, stripeConnected: false }, 'charge_card').href, '/settings?tab=payments')
  assert.equal(option({ ...ready, smtpConfigured: false }, 'send_link').href, '/settings?tab=email')
  assert.equal(option({ ...ready, paymentPageSet: false }, 'send_link').href, '/settings?tab=email')
  assert.equal(option({ ...ready, customerEmail: '' }, 'send_link').href, undefined)
  assert.deepEqual(option({ ...ready, card: null }, 'charge_card'), {
    value: 'charge_card', label: 'Charge their saved card', state: 'unavailable', note: 'The customer has no saved card.',
  })
  assert.equal(option({ ...ready, stripeConnected: false }, 'charge_card').state, 'unavailable')
  assert.equal(option({ ...ready, card: { card_last4: '0005' } }, 'charge_card').label, 'Charge the card ending 0005')
})

test('an option waits while what it needs is loading, and is never offered on a guess', () => {
  assert.equal(option({ ...ready, smtpConfigured: undefined }, 'send_link').state, 'checking')
  assert.equal(option({ ...ready, card: undefined }, 'charge_card').state, 'checking')
  // A fact already known to be missing wins over one still loading.
  assert.equal(option({ ...ready, customerEmail: '', stripeConnected: undefined }, 'send_link').state, 'unavailable')
  // A read that failed says so, and offers nothing.
  assert.deepEqual([option({ ...ready, smtpConfigured: undefined, linkFactsFailed: true }, 'send_link').state, option({ ...ready, smtpConfigured: undefined, linkFactsFailed: true }, 'send_link').note],
    ['unavailable', "Couldn't check whether the link can be sent."])
  assert.equal(option({ ...ready, card: undefined, cardFactsFailed: true }, 'charge_card').note, "Couldn't check for a saved card.")
})

test('before a customer is picked, the options that need one wait for it', () => {
  const f: PayFacts = { ...ready, noCustomer: true, customerEmail: undefined, card: undefined }
  assert.deepEqual(payOptions(f).slice(0, 2).map(o => [o.state, o.note]), [['checking', 'Pick a customer first.'], ['checking', 'Pick a customer first.']])
})

test('recording a payment and deciding later are always offered', () => {
  const none: PayFacts = { customerEmail: '', smtpConfigured: false, paymentPageSet: false, stripeConnected: false, livemode: false, card: null }
  assert.deepEqual(payOptions(none).filter(o => o.state === 'available').map(o => o.value), ['received', 'later'])
})

test('each choice opens its own confirm on the top-up page; deciding later opens none', () => {
  assert.equal(confirmAfterCreate('send_link'), 'email')
  assert.equal(confirmAfterCreate('charge_card'), 'collect')
  assert.equal(confirmAfterCreate('received'), 'record')
  assert.equal(confirmAfterCreate('later'), null)
})

test('the invoice page opens a confirm from its address only on an unpaid top-up', () => {
  const topup = { status: 'draft', billing_reason: 'topup' }
  assert.equal(arrivalConfirm('collect', topup), 'collect')
  assert.equal(arrivalConfirm('refund', topup), null)
  assert.equal(arrivalConfirm(null, topup), null)
  assert.equal(arrivalConfirm('collect', { status: 'finalized', billing_reason: 'manual' }), null)
  assert.equal(arrivalConfirm('record', { status: 'paid', billing_reason: 'topup' }), null)
})

test('the credit defaults to the price, and each amount must be above 0', () => {
  assert.deepEqual(sellCreditAmounts('90', ''), { ok: true, payCents: 9000, creditCents: 9000 })
  assert.deepEqual(sellCreditAmounts('90', '100.50'), { ok: true, payCents: 9000, creditCents: 10050 })
  assert.deepEqual(sellCreditAmounts('', ''), { ok: false, errors: { pay: 'Enter an amount like 100 or 100.50' } })
  assert.deepEqual(sellCreditAmounts('90', '0'), { ok: false, errors: { credit: 'Enter an amount greater than 0' } })
  assert.deepEqual(sellCreditAmounts('9,50', '1e3'), { ok: false, errors: { pay: 'Enter an amount like 100 or 100.50', credit: 'Enter an amount like 100 or 100.50' } })
})

test('a credit that differs from the price is said in one line', () => {
  assert.equal(discountLine(9000, 10000, money), 'They get $10.00 more credit than they pay.')
  assert.equal(discountLine(10000, 9000, money), 'They pay $10.00 more than the credit they get.')
  assert.equal(discountLine(9000, 9000, money), null)
})

test('the validity reads as the payment page reads it, in days', () => {
  assert.equal(creditValidityText(null), 'It never expires.')
  assert.equal(creditValidityText(undefined), 'It never expires.')
  assert.equal(creditValidityText(1), 'It is valid for 1 day after payment.')
  assert.equal(creditValidityText(365), 'It is valid for 365 days after payment.')
})
