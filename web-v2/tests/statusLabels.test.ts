// One status vocabulary: each state has one label and one tone everywhere.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { genericStatus, humanizeStatus, invoicePaymentChip, invoiceStatus, subscriptionFlags, subscriptionStatus } from '../src/lib/statusLabels.ts'

const now = Date.parse('2026-10-16T12:00:00Z')

test('invoiceStatus folds lifecycle and payment into one Stripe-style badge', () => {
  assert.deepEqual(invoiceStatus({ status: 'draft', payment_status: 'pending' }, now), { label: 'Draft', tone: 'neutral' })
  assert.deepEqual(invoiceStatus({ status: 'finalized', payment_status: 'pending', due_at: '2026-10-31T00:00:00Z' }, now), { label: 'Open', tone: 'info' })
  assert.deepEqual(invoiceStatus({ status: 'finalized', payment_status: 'pending', due_at: '2026-09-12T00:00:00Z' }, now), { label: 'Past due', tone: 'danger' })
  assert.deepEqual(invoiceStatus({ status: 'finalized', payment_status: 'failed' }, now), { label: 'Payment failed', tone: 'danger' })
  assert.deepEqual(invoiceStatus({ status: 'finalized', payment_status: 'processing' }, now), { label: 'Processing', tone: 'info' })
  assert.deepEqual(invoiceStatus({ status: 'paid', payment_status: 'succeeded' }, now), { label: 'Paid', tone: 'success' })
  // A voided invoice's leftover payment_status ("pending") must not leak.
  assert.deepEqual(invoiceStatus({ status: 'voided', payment_status: 'pending' }, now), { label: 'Void', tone: 'neutral' })
  assert.deepEqual(invoiceStatus({ status: 'uncollectible', payment_status: 'pending' }, now), { label: 'Uncollectible', tone: 'danger' })
})

test('Past due follows the given now (a test clock), not the wall clock', () => {
  const inv = { status: 'finalized', payment_status: 'pending', due_at: '2027-01-15T00:00:00Z' }
  assert.equal(invoiceStatus(inv, Date.parse('2027-01-14T00:00:00Z')).label, 'Open')
  assert.equal(invoiceStatus(inv, Date.parse('2027-01-16T00:00:00Z')).label, 'Past due')
})

test('subscriptionStatus and flags: paused collection and scheduled cancel are visible', () => {
  assert.deepEqual(subscriptionStatus({ status: 'active' }), { label: 'Active', tone: 'success' })
  assert.deepEqual(subscriptionStatus({ status: 'canceled' }), { label: 'Canceled', tone: 'neutral' })
  assert.deepEqual(subscriptionFlags({ status: 'active', pause_collection: { behavior: 'keep_as_draft' } }), [{ label: 'Collection paused', tone: 'warning' }])
  assert.deepEqual(subscriptionFlags({ status: 'trialing', cancel_at_period_end: true }), [{ label: 'Cancel scheduled', tone: 'warning' }])
  // A canceled subscription carries no live flags.
  assert.deepEqual(subscriptionFlags({ status: 'canceled', pause_collection: {} }), [])
})

test('generic codes read as sentence case, never snake_case', () => {
  assert.equal(humanizeStatus('past_due'), 'Past due')
  assert.deepEqual(genericStatus('no_endpoints'), { label: 'No endpoint', tone: 'neutral' })
  assert.deepEqual(genericStatus('escalated'), { label: 'Escalated', tone: 'warning' })
  assert.deepEqual(genericStatus('some_new_state'), { label: 'Some new state', tone: 'neutral' })
})

test('invoicePaymentChip never repeats what the invoice badge already says', () => {
  // Badge "Payment failed" / "Processing" / "Paid": no chip beside it.
  assert.equal(invoicePaymentChip({ status: 'finalized', payment_status: 'failed' }), null)
  assert.equal(invoicePaymentChip({ status: 'finalized', payment_status: 'processing' }), null)
  assert.equal(invoicePaymentChip({ status: 'finalized', payment_status: 'succeeded' }), null)
  assert.equal(invoicePaymentChip({ status: 'paid', payment_status: 'succeeded' }), null)
  // Drafts and voids have no payment to show.
  assert.equal(invoicePaymentChip({ status: 'draft', payment_status: 'pending' }), null)
  assert.equal(invoicePaymentChip({ status: 'voided', payment_status: 'pending' }), null)
  // A payment state the badge does not carry still shows.
  assert.deepEqual(invoicePaymentChip({ status: 'finalized', payment_status: 'pending' }), { label: 'Pending', tone: 'warning' })
  assert.deepEqual(invoicePaymentChip({ status: 'uncollectible', payment_status: 'failed' }), { label: 'Failed', tone: 'danger' })
})
