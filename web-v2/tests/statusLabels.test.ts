// One status vocabulary: each state has one label and one tone everywhere.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { dunningRunStatus, genericStatus, humanizeStatus, invoicePaymentChip, invoiceStatus, sentEmailStatus, subscriptionFlags, subscriptionStatus, trialBadgeEndsAt } from '../src/lib/statusLabels.ts'

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
  assert.deepEqual(subscriptionFlags({ status: 'active', pause_collection: { behavior: 'keep_as_draft' } }, 0), [{ label: 'Collection paused', tone: 'warning' }])
  assert.deepEqual(subscriptionFlags({ status: 'trialing', cancel_at_period_end: true }, 0), [{ label: 'Cancel scheduled', tone: 'warning' }])
  // A canceled subscription carries no live flags.
  assert.deepEqual(subscriptionFlags({ status: 'canceled', pause_collection: {} }, 0), [])
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
  // Drafts have no payment yet; a void or write-off is no longer collected,
  // so "Pending" or "Failed" beside it reads as a payment still expected.
  assert.equal(invoicePaymentChip({ status: 'draft', payment_status: 'pending' }), null)
  assert.equal(invoicePaymentChip({ status: 'voided', payment_status: 'pending' }), null)
  assert.equal(invoicePaymentChip({ status: 'uncollectible', payment_status: 'failed' }), null)
  assert.equal(invoicePaymentChip({ status: 'uncollectible', payment_status: 'pending' }), null)
  // A parked charge can be written off and is marked paid later if it
  // succeeded (docs/payments.md), so money may still arrive: keep "Unknown".
  assert.deepEqual(invoicePaymentChip({ status: 'uncollectible', payment_status: 'unknown' }), { label: 'Unknown', tone: 'neutral' })
  // A payment state the badge does not carry still shows.
  assert.deepEqual(invoicePaymentChip({ status: 'finalized', payment_status: 'pending' }), { label: 'Pending', tone: 'warning' })
  assert.deepEqual(invoicePaymentChip({ status: 'finalized', payment_status: 'unknown' }), { label: 'Unknown', tone: 'neutral' })
})

test('trialBadgeEndsAt: only a subscription still in its trial shows a trial badge', () => {
  const end = '2026-10-01T00:00:00Z'
  assert.equal(trialBadgeEndsAt({ status: 'trialing', trial_end_at: end }), end)
  // A trial that ended (or was ended now) leaves trial_end_at behind: an
  // active, paying subscription read a red "Expired" badge (sweep E5).
  for (const status of ['active', 'canceled', 'paused', 'draft']) {
    assert.equal(trialBadgeEndsAt({ status, trial_end_at: end }), null, status)
  }
  assert.equal(trialBadgeEndsAt({ status: 'trialing' }), null)
})

// A dunning run being chased is amber, never the green "active" means for a
// subscription; the words match the Dunning page's tabs.
test('a dunning run reads Active in amber, Escalated in red, Resolved in green', () => {
  assert.deepEqual(dunningRunStatus('active'), { label: 'Active', tone: 'warning' })
  assert.deepEqual(dunningRunStatus('escalated'), { label: 'Escalated', tone: 'danger' })
  assert.deepEqual(dunningRunStatus('resolved'), { label: 'Resolved', tone: 'success' })
  assert.deepEqual(genericStatus('active'), { label: 'Active', tone: 'success' }, 'control: a subscription or customer stays green')
})

// A sent email says what the mail provider reported, else where it is in the
// outbox, in words a reader knows: never "dispatched".
test('a sent email reads Queued, Sent, Delivered, Bounced, Marked as spam, Failed', () => {
  assert.deepEqual(sentEmailStatus({ status: 'pending' }), { label: 'Queued', tone: 'neutral' })
  assert.deepEqual(sentEmailStatus({ status: 'dispatched' }), { label: 'Sent', tone: 'info' })
  assert.deepEqual(sentEmailStatus({ status: 'dispatched', delivery_state: 'unknown' }), { label: 'Sent', tone: 'info' })
  assert.deepEqual(sentEmailStatus({ status: 'dispatched', delivery_state: 'delivered' }), { label: 'Delivered', tone: 'success' })
  assert.deepEqual(sentEmailStatus({ status: 'dispatched', delivery_state: 'bounced' }), { label: 'Bounced', tone: 'danger' })
  assert.deepEqual(sentEmailStatus({ status: 'dispatched', delivery_state: 'complained' }), { label: 'Marked as spam', tone: 'danger' })
  assert.deepEqual(sentEmailStatus({ status: 'failed' }), { label: 'Failed', tone: 'danger' })
  assert.deepEqual(sentEmailStatus({ status: 'skipped' }), { label: 'Not sent: settled first', tone: 'neutral' })
})

test('credit notes and invitations have words and tones', () => {
  assert.deepEqual(genericStatus('issued'), { label: 'Issued', tone: 'info' })
  assert.deepEqual(genericStatus('voided'), { label: 'Void', tone: 'neutral' })
  assert.deepEqual(genericStatus('accepted'), { label: 'Accepted', tone: 'success' })
  assert.deepEqual(genericStatus('paused'), { label: 'Paused', tone: 'warning' })
})
