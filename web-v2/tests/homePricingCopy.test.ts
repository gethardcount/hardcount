// Wording on the plan, meter, Dashboard and Usage pages that depends on
// state. Each version gets its own case.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import {
  planLockedSentence, planPricesNotice, priceRemoveSentence, priceWriteToast,
} from '../src/lib/pricingCopy.ts'
import { formatQuantity, quantityWithUnit, unitFor } from '../src/lib/usageQuantity.ts'
import { setupChecklistState, showSimulatedTotalsNote, usageNoneYet } from '../src/lib/homeNotices.ts'

test('plan lock names the server count, singular and plural, same words as the refusal', () => {
  assert.equal(
    planLockedSentence(1),
    '1 subscription uses this plan, so its base fee is fixed. Create a new plan with the new fee instead.',
  )
  assert.match(planLockedSentence(2), /^2 subscriptions use this plan,/)
  assert.match(planLockedSentence(1200), /^1,200 subscriptions use/)
})

test('price changes say when they apply, before saving', () => {
  assert.match(planPricesNotice(true), /^Price changes apply from each subscriber's next billing period\./)
  // A plan is priced where the subscription first joined it in the period
  // (ADR-130 §4 step 3, amended 2026-10-26).
  assert.match(planPricesNotice(true), /joins this plan after a change, by subscribing, moving to it or adding it, pays the new prices from then, unless the same subscription was on this plan earlier in that billing period\. On a test clock, advance the clock first\./)
  assert.doesNotMatch(planPricesNotice(true), /midnight|later today|next period/)
  // The pin belongs to the subscription: a customer that cancels and
  // subscribes again is a new join.
  assert.doesNotMatch(planPricesNotice(true), /unless they were/)
  assert.match(planPricesNotice(false), /apply at once/)
})

test('the toast follows the server answer', () => {
  assert.equal(priceWriteToast({ effective: 'now' }), 'Price saved.')
  assert.equal(priceWriteToast({ effective: 'next_period' }), "Price saved, from each subscriber's next billing period.")
  assert.equal(
    priceWriteToast({ effective: 'next_period', removed: true, ended_overrides: [{}, {}] }),
    "Price removed, from each subscriber's next billing period. 2 customer overrides ended.",
  )
})

test('removing a price names the overrides it ends', () => {
  assert.equal(
    priceRemoveSentence(false, 0),
    "It is removed at once. Its events go to another price of the plan that matches them, or aren't billed if none does. Adding the price back later doesn't bring the overrides back.",
  )
  assert.match(priceRemoveSentence(true, 0), /^From each subscriber's next billing period/)
  assert.match(priceRemoveSentence(true, 0), /joins this plan after the removal, by subscribing, moving to it or adding it, doesn't pay this price, unless the same subscription was on this plan earlier in that billing period\. On a test clock, advance the clock first\./)
  assert.doesNotMatch(priceRemoveSentence(true, 0), /midnight|later today|still pays|unless they were/)
  assert.match(priceRemoveSentence(true, 1), /ends 1 customer override of it/)
  assert.match(priceRemoveSentence(true, 3), /ends 3 customer overrides of it/)
  assert.match(priceRemoveSentence(true, null), /ends any customer override/)
})

test('quantities get thousands separators and stay exact', () => {
  assert.equal(formatQuantity('1234567.000000000000'), '1,234,567')
  assert.equal(formatQuantity('1234.500000000000'), '1,234.5')
  assert.equal(formatQuantity('0.000000000001'), '0.000000000001')
  assert.equal(formatQuantity('999'), '999')
  assert.equal(formatQuantity('0.000000000000'), '0')
  assert.equal(formatQuantity('-1500'), '-1,500')
  assert.equal(formatQuantity('n/a'), 'n/a')
})

test('units read plural for any quantity but 1, and typed abbreviations stay as typed', () => {
  assert.equal(unitFor('1', 'call'), 'call')
  assert.equal(unitFor('2', 'call'), 'calls')
  assert.equal(unitFor('0', 'call'), 'calls')
  assert.equal(unitFor('2', 'query'), 'queries')
  assert.equal(unitFor('2', 'batch'), 'batches')
  assert.equal(unitFor('2', 'tokens'), 'tokens')
  assert.equal(unitFor('2', 'GB'), 'GB')
  assert.equal(unitFor('2', ''), '')
  assert.equal(quantityWithUnit('12500.000000000000', 'token'), '12,500 tokens')
  assert.equal(quantityWithUnit('1.000000000000', 'token'), '1 token')
})

test('Dashboard note shows only in test mode with test clocks', () => {
  assert.equal(showSimulatedTotalsNote(false, 1), true)
  assert.equal(showSimulatedTotalsNote(false, 0), false)
  assert.equal(showSimulatedTotalsNote(true, 3), false)
})

test('Usage "Set up a meter" only when no meter exists', () => {
  assert.equal(usageNoneYet(0).setUpMeter, true)
  assert.equal(usageNoneYet(2).setUpMeter, false)
  assert.equal(usageNoneYet(null).setUpMeter, false)
  assert.doesNotMatch(usageNoneYet(2).description, /create one/)
})

test('"Show setup checklist" is offered only when it would bring the pill back', () => {
  // hidden with a step left: offered, and the pill stays away until clicked
  assert.deepEqual(setupChecklistState(true, true, 3, 5), { showPill: false, offerShowAgain: true })
  // hidden with every step done: clicking would show nothing, so no item
  assert.deepEqual(setupChecklistState(true, true, 5, 5), { showPill: false, offerShowAgain: false })
  // not hidden: the pill shows while a step is left, never the menu item
  assert.deepEqual(setupChecklistState(false, true, 3, 5), { showPill: true, offerShowAgain: false })
  assert.deepEqual(setupChecklistState(false, true, 5, 5), { showPill: false, offerShowAgain: false })
  // still loading: neither
  assert.deepEqual(setupChecklistState(true, false, 0, 5), { showPill: false, offerShowAgain: false })
})
