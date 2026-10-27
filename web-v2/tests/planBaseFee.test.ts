// The Edit plan dialog's base fee and billing timing. The server refuses a
// base of 0 with in_advance, and fixes the timing once a subscription has
// used the plan, so a used in_advance plan keeps a fee above 0. The dialog
// showed the timing as "At end of period" and said "0 for no base fee".
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { editPlanBaseFee } from '../src/lib/planBaseFee.ts'

const usedInAdvance = { chosenTiming: 'in_advance', storedTiming: 'in_advance', used: true, interval: 'monthly' } as const

test('a used in_advance plan shows its own timing and keeps a fee above 0', () => {
  const zero = editPlanBaseFee({ ...usedInAdvance, baseCents: 0 })
  assert.equal(zero.timing, 'in_advance')
  assert.equal(zero.timingDisabled, true)
  assert.equal(zero.baseHint, 'Monthly recurring charge, above 0.')
  assert.equal(zero.baseError, 'This plan bills its base fee at the start of each period, so the fee must be above 0. To charge no base fee, create a new plan.')
  assert.equal(zero.timingHint, 'Customer charged the base fee on day 1 of each period; usage settles at period end. Fixed once a subscription has used the plan.')

  const fee = editPlanBaseFee({ ...usedInAdvance, baseCents: 900 })
  assert.equal(fee.timing, 'in_advance')
  assert.equal(fee.baseError, null)
  assert.equal(fee.baseHint, 'Monthly recurring charge, above 0.')
})

test('a plan no subscription has used can go to 0, and then bills at the end of each period', () => {
  const zero = editPlanBaseFee({ ...usedInAdvance, used: false, baseCents: 0, interval: 'yearly' })
  assert.equal(zero.timing, 'in_arrears')
  assert.equal(zero.timingDisabled, true)
  assert.equal(zero.baseError, null)
  assert.equal(zero.baseHint, 'Yearly recurring charge. 0 for no base fee.')
  assert.equal(zero.timingHint, 'A plan with no base fee bills its usage at the end of each period.')

  const fee = editPlanBaseFee({ ...usedInAdvance, used: false, baseCents: 900, chosenTiming: 'in_arrears' })
  assert.deepEqual([fee.timing, fee.timingDisabled, fee.baseError], ['in_arrears', false, null])
})

test('a used in_arrears plan can go to 0', () => {
  const zero = editPlanBaseFee({ baseCents: 0, chosenTiming: 'in_arrears', storedTiming: 'in_arrears', used: true, interval: 'monthly' })
  assert.deepEqual([zero.timing, zero.timingDisabled, zero.baseError], ['in_arrears', true, null])
  assert.equal(zero.baseHint, 'Monthly recurring charge. 0 for no base fee.')
  assert.equal(zero.timingHint, 'A plan with no base fee bills its usage at the end of each period. Fixed once a subscription has used the plan.')
})
