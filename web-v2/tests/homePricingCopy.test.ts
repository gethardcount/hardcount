// Wording on the plan, meter, Dashboard and Usage pages that depends on
// state. Each version gets its own case.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  basePriceZeroBlocked, deleteMatchedPriceSentence, planLockedSentence,
} from '../src/lib/pricingCopy.ts'
import { formatQuantity, quantityWithUnit, unitFor } from '../src/lib/usageQuantity.ts'
import { setupChecklistState, showSimulatedTotalsNote, usageNoneYet } from '../src/lib/homeNotices.ts'

test('plan lock names the server count, singular and plural, same words as the refusal', () => {
  assert.equal(
    planLockedSentence(1),
    '1 subscription uses this plan, so its price, billing timing and meters are locked. Create a new plan with the new terms instead.',
  )
  assert.match(planLockedSentence(2), /^2 subscriptions use this plan,/)
  assert.match(planLockedSentence(1200), /^1,200 subscriptions use/)
})

test('base price back to 0 is blocked only when it is above 0 today', () => {
  assert.equal(basePriceZeroBlocked('0', 4900), true)
  assert.equal(basePriceZeroBlocked('', 4900), true)
  assert.equal(basePriceZeroBlocked('0.001', 4900), true) // rounds to 0 cents
  assert.equal(basePriceZeroBlocked('49', 4900), false)
  assert.equal(basePriceZeroBlocked('0', 0), false) // already 0: nothing to change
})

test('deleting a matched price says where its events go', () => {
  assert.equal(
    deleteMatchedPriceSentence('model=gpt-4', true),
    "Events it matched (model=gpt-4) move to another matching price or the default price from the next invoice. Finalized invoices don't change.",
  )
  assert.equal(
    deleteMatchedPriceSentence('model=gpt-4', false),
    "Events it matched (model=gpt-4) go unbilled from the next invoice unless another price below matches them. Finalized invoices don't change.",
  )
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
