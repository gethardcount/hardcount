// The subscription page shows at most one notice box (the most urgent that
// applies); every other notice is a one-line row that keeps its button.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import {
  NO_CARD_SENTENCE,
  NO_CARD_TITLE,
  billingFailureNextLine,
  billingFailureTitle,
  collectionPausedSentence,
  subscriptionNoticeLayout,
} from '../src/lib/subscriptionNotices.ts'

const none = { billingFailure: false, needsCard: false, collectionPaused: false }

test('a healthy subscription shows no notice', () => {
  assert.deepEqual(subscriptionNoticeLayout(none), { box: null, rows: [] })
})

test('a single notice is the box, with no rows', () => {
  assert.deepEqual(subscriptionNoticeLayout({ ...none, collectionPaused: true }), { box: 'collection_paused', rows: [] })
  assert.deepEqual(subscriptionNoticeLayout({ ...none, needsCard: true }), { box: 'no_card', rows: [] })
  assert.deepEqual(subscriptionNoticeLayout({ ...none, billingFailure: true }), { box: 'billing_failure', rows: [] })
})

test('order: billing failure, then no card, then collection paused', () => {
  const all = { billingFailure: true, needsCard: true, collectionPaused: true }
  assert.deepEqual(subscriptionNoticeLayout(all), {
    box: 'billing_failure',
    rows: ['no_card', 'collection_paused'],
  })
  assert.deepEqual(subscriptionNoticeLayout({ ...all, billingFailure: false }), {
    box: 'no_card',
    rows: ['collection_paused'],
  })
  assert.deepEqual(subscriptionNoticeLayout({ ...none, billingFailure: true, collectionPaused: true }), {
    box: 'billing_failure',
    rows: ['collection_paused'],
  })
})

test('never more than one box', () => {
  for (let mask = 0; mask < 8; mask++) {
    const s = {
      billingFailure: !!(mask & 1),
      needsCard: !!(mask & 2),
      collectionPaused: !!(mask & 4),
    }
    const l = subscriptionNoticeLayout(s)
    const count = Object.values(s).filter(Boolean).length
    assert.equal(l.box === null ? 0 : 1, count === 0 ? 0 : 1)
    assert.equal(l.rows.length, Math.max(0, count - 1))
    assert.ok(l.box === null || !l.rows.includes(l.box))
  }
})

test('billing failure: attempt count in the title, next try or stop after the reason', () => {
  assert.equal(billingFailureTitle({ parked: false, attempts: 2, max_attempts: 5 }), 'Billing failed (attempt 2 of 5)')
  assert.equal(billingFailureTitle({ parked: true, attempts: 5, max_attempts: 5 }), 'Billing has stopped')
  assert.equal(billingFailureNextLine({ parked: false, attempts: 2 }, 'Oct 15, 2026, 10:00 AM'), 'Next automatic try after Oct 15, 2026, 10:00 AM.')
  assert.equal(billingFailureNextLine({ parked: false, attempts: 2 }), null)
  assert.equal(billingFailureNextLine({ parked: true, attempts: 5 }, 'ignored'), 'It failed 5 times. No invoice is created until you retry.')
  assert.equal(billingFailureNextLine({ parked: true, attempts: 1 }), 'It failed 1 time. No invoice is created until you retry.')
})

test('no-card notice: one title on both pages, no "manual follow-up" promise', () => {
  assert.equal(NO_CARD_TITLE, 'No payment method')
  assert.equal(NO_CARD_SENTENCE, "Renewals can't be charged until the customer adds a card.")
  assert.doesNotMatch(NO_CARD_SENTENCE, /manual/)
})

test('collection paused: names the auto-resume date only when there is one', () => {
  assert.equal(collectionPausedSentence(), 'New invoices are saved as drafts and not charged.')
  assert.equal(
    collectionPausedSentence('Nov 1'),
    'New invoices are saved as drafts and not charged. Collection resumes Nov 1.',
  )
})
