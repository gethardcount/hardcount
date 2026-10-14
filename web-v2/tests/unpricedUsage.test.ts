// The cost dashboard's unpriced-usage notice is built from the unmatched
// rows, not from the server's lowercase engine sentence.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { UNPRICED_SENTENCE, UNPRICED_TITLE, meterQuantityLine, unpricedLine, unpricedMeters } from '../src/lib/unpricedUsage.ts'

const meter = (id: string, rules: { unmatched?: boolean; quantity: string }[]) => ({
  meter_id: id, meter_name: `Meter ${id}`, meter_key: id, unit: 'tokens', rules,
})

test('only meters with unmatched usage are listed, with the exact unbilled quantity', () => {
  const got = unpricedMeters([
    meter('a', [{ quantity: '100' }, { unmatched: true, quantity: '1250.5' }]),
    meter('b', [{ quantity: '7' }]),
    meter('c', [{ unmatched: true, quantity: '0' }]),
  ])
  assert.deepEqual(got, [{ meterId: 'a', meterName: 'Meter a', unit: 'tokens', quantity: 1250.5 }])
  assert.equal(unpricedLine(got[0]), '1,250.5 tokens on Meter a not billed.')
})

test('no unmatched rows means no notice', () => {
  assert.deepEqual(unpricedMeters([meter('a', [{ quantity: '5' }])]), [])
})

test('the notice is plain words in the notice shape', () => {
  assert.equal(UNPRICED_TITLE, 'Some usage has no price')
  assert.ok(UNPRICED_TITLE.split(' ').length <= 6)
  assert.doesNotMatch(UNPRICED_SENTENCE, /pricing rule|meter "/)
  assert.ok(UNPRICED_SENTENCE.split(' ').length <= 25)
})

// The meter row under the notice must not say "0 tokens · 0 rules" when the
// meter's only usage had no price (total_quantity counts billed usage only).
test('a meter whose usage all had no price says what was and was not billed', () => {
  const line = meterQuantityLine({ unit: 'tokens', total_quantity: '0', rules: [{ unmatched: true, quantity: '12400' }] })
  assert.equal(line, '0 tokens billed · 12,400 tokens not billed · no price matched')
  // Same number, same wording as the notice above the row.
  assert.ok(line.includes('12,400 tokens'))
  assert.equal(unpricedLine(unpricedMeters([{ ...meter('m', [{ unmatched: true, quantity: '12400' }]) }])[0]), '12,400 tokens on Meter m not billed.')
})

test('a meter with some usage priced counts its prices', () => {
  assert.equal(
    meterQuantityLine({ unit: 'tokens', total_quantity: '5000', rules: [{ quantity: '5000' }, { unmatched: true, quantity: '250.5' }] }),
    '5,000 tokens billed · 250.5 tokens not billed · 1 price',
  )
})

test('fully priced meters read as before, with separators', () => {
  assert.equal(meterQuantityLine({ unit: 'tokens', total_quantity: '1234567', rules: [{ quantity: '1234567' }] }), '1,234,567 tokens')
  assert.equal(
    meterQuantityLine({ unit: 'calls', total_quantity: '30', rules: [{ quantity: '10' }, { quantity: '20' }] }),
    '30 calls · 2 prices',
  )
  assert.equal(meterQuantityLine({ unit: '', total_quantity: '7', rules: [{ quantity: '7' }] }), '7')
})
