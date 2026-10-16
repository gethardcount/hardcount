// The Settings → Tax worked example. Every amount is formatted the same way
// in the workspace currency; the base used to take its symbol from another
// list, so a CAD workspace read "$100.00 … = CA$118.00".
import { test, beforeEach } from 'vitest'
import assert from 'node:assert/strict'
import { setActiveCurrency } from '../src/lib/money.ts'
import { taxExampleAmounts } from '../src/lib/taxExample.ts'

beforeEach(() => setActiveCurrency('USD'))

test('exclusive: tax added on top, all in the workspace currency', () => {
  assert.deepEqual(taxExampleAmounts(18, false, 'CAD'), { base: 'CA$100.00', net: 'CA$100.00', total: 'CA$118.00' })
})

test('inclusive: tax carved out of the sticker price', () => {
  // 10000 × 10000 / 11800 = 8474.57… → 8475 cents
  assert.deepEqual(taxExampleAmounts(18, true, 'EUR'), { base: '€100.00', net: '€84.75', total: '€100.00' })
})

test('fractional rate rounds to whole cents', () => {
  assert.equal(taxExampleAmounts(8.875, false, 'USD').total, '$108.88')
})
