// Guards the per-meter rows on the Usage page: each number is named by the
// meter's aggregation (a peak never reads as a total) and the daily
// sparkline has one point per calendar day, zero on quiet days.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { addDays, fillDays, meterValueCaption, meterModeBadge, sparklinePoints } from '../src/lib/usageSummary.ts'

test('caption names the aggregation, never "total" for a peak', () => {
  assert.equal(meterValueCaption('max', 2), 'Peak reading per customer, added across 2 customers')
  assert.equal(meterValueCaption('max', 1), 'Peak reading')
  assert.equal(meterValueCaption('last', 3), 'Latest reading per customer, added across 3 customers')
  assert.equal(meterValueCaption('count', 5), 'Number of events')
  assert.equal(meterValueCaption('sum', 5), 'Sum of reported values')
  assert.match(meterValueCaption('', 1), /unknown/)
})

test('mode badge uses the shared aggregation labels', () => {
  assert.equal(meterModeBadge('max'), 'Maximum')
  assert.equal(meterModeBadge('last'), 'Latest value')
  assert.equal(meterModeBadge(''), 'Sum')
})

test('addDays crosses month and year ends on civil dates', () => {
  assert.equal(addDays('2026-02-28', 1), '2026-03-01')
  assert.equal(addDays('2026-12-31', 1), '2027-01-01')
  assert.equal(addDays('2026-03-01', -1), '2026-02-28')
})

test('fillDays zero-fills quiet days across the window', () => {
  const daily = [{ date: '2026-03-02', value: '5' }, { date: '2026-03-04', value: '1.5' }]
  assert.deepEqual(fillDays(daily, '2026-03-01', '2026-03-05'), [0, 5, 0, 1.5, 0])
})

test('fillDays without a window spans first to last event day', () => {
  const daily = [{ date: '2026-03-04', value: '2' }, { date: '2026-03-02', value: '1' }]
  assert.deepEqual(fillDays(daily), [1, 0, 2])
  assert.deepEqual(fillDays([]), [])
})

test('fillDays with no end date runs to today, zero on quiet recent days', () => {
  const daily = [{ date: '2026-03-02', value: '4' }]
  assert.deepEqual(fillDays(daily, '2026-03-01', '', '2026-03-05'), [0, 4, 0, 0, 0])
  assert.deepEqual(fillDays(daily, undefined, undefined, '2026-03-04'), [4, 0, 0])
  // A later event day still ends the line.
  assert.deepEqual(fillDays([{ date: '2026-03-06', value: '1' }], '2026-03-05', '', '2026-03-05'), [0, 1])
  // An explicit end date wins over today.
  assert.deepEqual(fillDays(daily, '2026-03-01', '2026-03-02', '2026-03-09'), [0, 4])
})

test('a mixed meter is named as a raw sum that billing may count differently', () => {
  assert.equal(meterModeBadge('mixed'), 'Mixed')
  assert.match(meterValueCaption('mixed', 3), /^Sum of reported values\. .*may differ\.$/)
})

test('fillDays keeps the newest days when the window is too long', () => {
  const daily = [{ date: '2026-03-10', value: '9' }]
  const out = fillDays(daily, '2020-01-01', '2026-03-10', undefined, 5)
  assert.deepEqual(out, [0, 0, 0, 0, 9])
})

test('sparklinePoints scales to the box from a zero baseline', () => {
  assert.equal(sparklinePoints([0, 10], 100, 20), '0,20 100,0')
  assert.equal(sparklinePoints([3, 3, 3], 100, 20), '0,0 50,0 100,0')
  assert.equal(sparklinePoints([0, 0], 100, 20), '0,20 100,20')
  assert.equal(sparklinePoints([], 100, 20), '')
})
