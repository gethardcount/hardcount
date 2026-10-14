// No raw codes on screen. The invoice notice showed "payment_anomaly" as its
// label and "payment.no_payment_method" in grey under the buttons; the
// invoice list's hover showed "no_payment_method — <whole paragraph>"; the
// list filter chip read "payment: unknown".
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { attentionReasonLabel, supportCodeLine } from '../src/lib/attentionLabels.ts'
import { paymentStatusLabel } from '../src/lib/status.ts'

test('payment_anomaly has a label', () => {
  assert.equal(attentionReasonLabel('payment_anomaly'), 'Payment needs review')
})

test('a parked invoice is told apart by its code', () => {
  assert.equal(attentionReasonLabel('payment_unconfirmed', 'payment.unidentifiable'), 'Payment status unknown')
  assert.equal(attentionReasonLabel('payment_unconfirmed', 'payment.unconfirmed'), 'Confirming payment')
})

test('the payment_processing cases are told apart by code', () => {
  assert.equal(attentionReasonLabel('payment_processing', 'payment.processing'), 'Payment processing')
  assert.equal(attentionReasonLabel('payment_processing', 'payment.page_open'), 'Customer is paying')
  assert.equal(attentionReasonLabel('payment_processing', 'payment.recovery_processing'), 'Payment in progress')
  assert.equal(attentionReasonLabel('payment_scheduled', 'payment.scheduled'), 'Payment scheduled')
})

test('an unmapped reason never shows the raw code', () => {
  const label = attentionReasonLabel('brand_new_reason')
  assert.equal(label, 'Needs attention')
  assert.doesNotMatch(label, /_/)
})

test('the support code moves into Details with a label', () => {
  assert.equal(supportCodeLine('payment.no_payment_method'), 'Support code: payment.no_payment_method')
  assert.equal(supportCodeLine(undefined), null)
})

test('payment status filter chip reads in words', () => {
  assert.equal(paymentStatusLabel('unknown'), 'Payment status unknown')
  assert.equal(paymentStatusLabel('failed'), 'Payment failed')
  assert.equal(paymentStatusLabel('succeeded'), 'Paid')
  assert.equal(paymentStatusLabel('requires_action'), 'Payment requires action')
})
