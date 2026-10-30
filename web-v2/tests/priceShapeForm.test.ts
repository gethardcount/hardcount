// The price form refuses what it cannot read, under the box that holds it:
// a package of 0 units, a negative rate, "9,50". None is rounded or filled in.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { buildPriceShape, type PriceShapeInput } from '../src/lib/priceShapeForm.ts'

const blank: PriceShapeInput = { mode: 'per_unit', unitAmount: '', tiers: [], packageSize: '', packageAmount: '' }

test('a per-unit rate is sent in cents, exactly', () => {
  assert.deepEqual(buildPriceShape({ ...blank, unitAmount: '0.002' }), { shape: { mode: 'per_unit', unit_amount_cents: '0.2' } })
  assert.deepEqual(buildPriceShape({ ...blank, unitAmount: '0' }), { shape: { mode: 'per_unit', unit_amount_cents: '0' } })
})

test('a blank, negative or comma rate is refused under its box', () => {
  assert.deepEqual(buildPriceShape(blank), { error: { field: 'unitAmount', message: 'Enter an amount, for example 0.01. Use 0 for free.' } })
  assert.deepEqual(buildPriceShape({ ...blank, unitAmount: '-1' }), { error: { field: 'unitAmount', message: "Enter 0 or more. An amount can't be negative." } })
  assert.deepEqual(buildPriceShape({ ...blank, unitAmount: '9,50' }), { error: { field: 'unitAmount', message: 'Use a point for decimals, not a comma: for example 0.01.' } })
})

test('a package of 0 units is refused, never read as 1', () => {
  const pkg = { ...blank, mode: 'package', packageAmount: '10' }
  for (const size of ['0', '', '2.5', '-3']) {
    assert.deepEqual(buildPriceShape({ ...pkg, packageSize: size }),
      { error: { field: 'packageSize', message: 'Enter a whole number greater than 0, for example 100.' } }, `size "${size}"`)
  }
  assert.deepEqual(buildPriceShape({ ...pkg, packageSize: '100' }), { shape: { mode: 'package', package_size: 100, package_amount_cents: '1000' } })
})

test('a tier error names its tier and its box', () => {
  const tiers = (upTo: string, price: string) => ({ ...blank, mode: 'graduated', tiers: [{ upTo, price }, { upTo: '', price: '0.05' }] })
  // "Up to" 100.9 was saved as 100.
  assert.deepEqual(buildPriceShape(tiers('100.9', '0.10')),
    { error: { field: 'tierUpTo', tier: 0, message: 'Tier 1: enter how many units the tier goes up to, a whole number greater than 0.' } })
  assert.deepEqual(buildPriceShape(tiers('100', '-0.10')),
    { error: { field: 'tierPrice', tier: 0, message: "Enter 0 or more. An amount can't be negative." } })
  assert.deepEqual(buildPriceShape(tiers('100', '0.10')),
    { shape: { mode: 'graduated', tiers: [{ up_to: 100, unit_amount_cents: '10' }, { up_to: null, unit_amount_cents: '5' }] } })
})
