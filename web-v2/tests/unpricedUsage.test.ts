// The cost dashboard's unpriced-usage notice is built from the unmatched
// rows, not from the server's lowercase engine sentence.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { UNPRICED_SENTENCE, UNPRICED_TITLE, unpricedLine, unpricedMeters } from '../src/lib/unpricedUsage.ts'

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
