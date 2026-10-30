// Which buttons the invoice notice offers for a refund made in Stripe that
// Hardcount flagged (ADR-134), and what Record sends. A button the server
// would refuse is never offered; Record sends exactly the refund.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import {
  dismissNoteError,
  flaggedStripeRefunds,
  recordStripeRefundRequest,
  stripeRefundChoices,
  stripeRefundNoteReason,
  type StripeRefundNote,
  type StripeRefundRow,
} from '../src/lib/stripeRefunds.ts'

const refund = (over: Partial<StripeRefundRow> = {}): StripeRefundRow => ({
  stripe_refund_id: 're_1',
  amount_cents: 4000,
  currency: 'USD',
  status: 'succeeded',
  outcome: 'needs_review',
  review_reason: 'seen_late',
  ...over,
})
const paid = { status: 'paid', payment_status: 'succeeded' }
const failedNote = (over: Partial<StripeRefundNote> = {}): StripeRefundNote => ({
  id: 'cn_1', credit_note_number: 'CN-0001', status: 'issued', refund_status: 'failed', refund_amount_cents: 4000, ...over,
})

test('only flagged refunds get buttons', () => {
  const rows = ['awaiting', 'credit_note', 'other_payment', 'needs_review', 'not_refunded', 'dismissed']
    .map(outcome => refund({ outcome, stripe_refund_id: `re_${outcome}` }))
  assert.deepEqual(flaggedStripeRefunds(rows).map(r => r.stripe_refund_id), ['re_needs_review'])
  assert.deepEqual(flaggedStripeRefunds(undefined), [])
  assert.deepEqual(stripeRefundChoices(refund({ outcome: 'dismissed' }), paid, []), { attachTo: [], record: false, dismiss: false })
})

test('a refund seen late on a paid invoice can be recorded or dismissed', () => {
  assert.deepEqual(stripeRefundChoices(refund(), paid, []), { attachTo: [], record: true, dismiss: true })
})

test('a refund on an invoice that is not paid can be recorded only once it is paid', () => {
  const r = refund({ review_reason: 'invoice_not_paid' })
  assert.equal(stripeRefundChoices(r, { status: 'finalized', payment_status: 'pending' }, []).record, false)
  assert.equal(stripeRefundChoices(r, paid, []).record, true)
  assert.equal(stripeRefundChoices(r, { status: 'paid', payment_status: 'processing' }, []).record, false)
})

test('refunds the server refuses to record offer Dismiss only', () => {
  for (const review_reason of ['exceeds_card_refundable', 'exceeds_creditable', 'prepaid_commit', 'currency_mismatch']) {
    assert.deepEqual(stripeRefundChoices(refund({ review_reason }), paid, [failedNote()]), { attachTo: [], record: false, dismiss: true }, review_reason)
  }
})

test('a refund that no longer succeeded at Stripe is not recorded or attached', () => {
  for (const status of ['failed', 'canceled', 'pending']) {
    const c = stripeRefundChoices(refund({ status, review_reason: 'credit_note_refund_failed' }), paid, [failedNote()])
    assert.deepEqual(c, { attachTo: [], record: false, dismiss: true }, status)
  }
})

test('a refund that may replace a failed refund can be attached to that note, or recorded, or dismissed', () => {
  const notes = [
    failedNote(),
    failedNote({ id: 'cn_2', credit_note_number: 'CN-0002', refund_amount_cents: 3999 }), // another amount: the server refuses
    failedNote({ id: 'cn_3', credit_note_number: 'CN-0003', refund_status: 'succeeded' }),
    failedNote({ id: 'cn_4', credit_note_number: 'CN-0004', status: 'voided' }),
  ]
  const c = stripeRefundChoices(refund({ review_reason: 'credit_note_refund_failed' }), paid, notes)
  assert.deepEqual(c.attachTo.map(n => n.credit_note_number), ['CN-0001'])
  assert.equal(c.record, true)
  assert.equal(c.dismiss, true)
})

test('Attach is offered only for the reason the server flags it for', () => {
  assert.deepEqual(stripeRefundChoices(refund({ review_reason: 'seen_late' }), paid, [failedNote()]).attachTo, [])
})

test('Record sends the refund as the whole card refund of an issued note, nothing to the balance', () => {
  const body = recordStripeRefundRequest('inv_1', refund({ reason: 'requested_by_customer' }))
  assert.deepEqual(body, {
    invoice_id: 'inv_1',
    reason: 'Refunded in Stripe: requested by customer',
    refund_amount_cents: 4000,
    credit_amount_cents: 0,
    out_of_band_amount_cents: 0,
    auto_issue: true,
    stripe_refund_id: 're_1',
    lines: [{ description: 'Refund made in Stripe (re_1)', quantity: 1, unit_amount_cents: 4000 }],
  })
})

test('the recorded note reads like one Hardcount recorded from the webhook', () => {
  assert.equal(stripeRefundNoteReason(undefined), 'Refunded in Stripe')
  assert.equal(stripeRefundNoteReason('  '), 'Refunded in Stripe')
  assert.equal(stripeRefundNoteReason('duplicate'), 'Refunded in Stripe: duplicate')
})

test('the dismissal note is required and at most 1000 characters, as the server counts them', () => {
  assert.equal(dismissNoteError(''), 'Say how you handled this refund.')
  assert.equal(dismissNoteError('   '), 'Say how you handled this refund.')
  assert.equal(dismissNoteError('Recorded in our books'), '')
  assert.equal(dismissNoteError('a'.repeat(1000)), '')
  assert.equal(dismissNoteError(' ' + 'a'.repeat(1000) + ' '), '')
  assert.match(dismissNoteError('a'.repeat(1001)), /1000 characters/)
  // An emoji is two UTF-16 units but one character to the server.
  assert.equal(dismissNoteError('😀'.repeat(1000)), '')
})
