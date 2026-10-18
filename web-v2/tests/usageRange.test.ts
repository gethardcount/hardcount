// Guards the Usage Events window presets.
//
// The page used to send no time bound at all on first load, so its stat cards
// aggregated the tenant's entire event history every time. Bounding the default
// is the fix; these tests pin the two properties that make it safe:
//
//   1. an unknown ?range= falls back to the BOUNDED default, never to 'all'
//      (a typo'd URL must not silently reinstate the full-history scan), and
//   2. 'all' still resolves to a genuinely unbounded window, because bounding
//      the default must not remove the capability.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import {
  DEFAULT_RANGE,
  RANGE_KEYS,
  parseRange,
  rangeCaption,
  resolveWindow,
  usageExportWindow,
  parseDimensionFilter,
  type RangeKey,
  type Window,
} from '../src/lib/usageRange.ts'
import { startOfDayInTZ, endOfDayInTZ } from '../src/lib/dates.ts'

// Deterministic stand-in for the tenant-TZ date helper.
const fakeAgo = (amount: number, unit: 'day' | 'month') => `ago:${amount}${unit === 'day' ? 'd' : 'm'}`
const noCustom: Window = { from: '', to: '' }

test('the default range is bounded', () => {
  assert.notEqual(DEFAULT_RANGE, 'all')
  const w = resolveWindow(DEFAULT_RANGE, noCustom, fakeAgo)
  assert.notEqual(w.from, '', 'the default must carry a lower bound — that is the whole point')
})

test('unknown or missing ?range= falls back to the bounded default, not to all-time', () => {
  for (const raw of [null, undefined, '', 'all-time', 'ALL', '30', 'custom-range', '../etc/passwd']) {
    assert.equal(parseRange(raw), DEFAULT_RANGE, `input ${JSON.stringify(raw)}`)
  }
})

test('every declared range key round-trips through parseRange', () => {
  for (const k of RANGE_KEYS) assert.equal(parseRange(k), k)
})

test('presets bound the start and leave the end open', () => {
  const cases: Array<[RangeKey, string]> = [
    ['7d', 'ago:7d'],
    ['30d', 'ago:30d'],
    ['90d', 'ago:90d'],
    ['12m', 'ago:12m'],
  ]
  for (const [range, from] of cases) {
    const w = resolveWindow(range, noCustom, fakeAgo)
    assert.equal(w.from, from)
    assert.equal(w.to, '', 'a preset must not pin the end, or the window stops tracking forward')
  }
})

test('all-time resolves to a genuinely unbounded window', () => {
  assert.deepEqual(resolveWindow('all', noCustom, fakeAgo), { from: '', to: '' })
})

test('custom defers entirely to the operator dates and ignores the preset helper', () => {
  const custom: Window = { from: '2026-01-01', to: '2026-03-31' }
  assert.deepEqual(resolveWindow('custom', custom, fakeAgo), custom)
  // A preset must NOT leak the operator's stale custom dates through.
  assert.deepEqual(resolveWindow('7d', custom, fakeAgo), { from: 'ago:7d', to: '' })
})

test('the caption names the window, and never reads back the word "custom"', () => {
  assert.equal(rangeCaption('30d', { from: 'ago:30d', to: '' }), 'Totals for the last 30 days')
  assert.equal(rangeCaption('12m', { from: 'ago:12m', to: '' }), 'Totals for the last 12 months')
  assert.equal(rangeCaption('all', noCustom), 'Totals across all time')

  assert.equal(rangeCaption('custom', { from: '2026-01-01', to: '2026-03-31' }), 'Totals for 2026-01-01 to 2026-03-31')
  assert.equal(rangeCaption('custom', { from: '2026-01-01', to: '' }), 'Totals from 2026-01-01 onwards')
  assert.equal(rangeCaption('custom', { from: '', to: '2026-03-31' }), 'Totals up to 2026-03-31')
  // Custom with no dates IS unbounded — say so rather than implying a filter.
  assert.equal(rangeCaption('custom', noCustom), 'Totals across all time')

  for (const k of RANGE_KEYS) {
    assert.doesNotMatch(rangeCaption(k, noCustom), /custom/i, `caption for ${k} leaks the mode name`)
  }
})

// An empty page under the default 30-day window said "No usage events yet"
// to operators whose events were older.
import { usageEmptyKind, needsAnyEventCheck } from '../src/lib/usageRange.ts'

test('empty Usage page: the default 30-day window on a workspace with no events says none yet', () => {
  // The common first-use path: DEFAULT_RANGE, no filters, nothing ever sent.
  assert.equal(usageEmptyKind(DEFAULT_RANGE, {}, false), 'none-yet')
  assert.equal(needsAnyEventCheck(DEFAULT_RANGE, {}), true)
})

test('empty Usage page: older events exist, so the window hides them', () => {
  assert.equal(usageEmptyKind('30d', {}, true), 'no-match')
  assert.equal(usageEmptyKind('custom', {}, true), 'no-match')
})

test('empty Usage page: unproven "none yet" falls back to the claim that is always true', () => {
  // null: the check is pending or failed.
  assert.equal(usageEmptyKind('30d', {}, null), 'no-match')
})

test('empty Usage page: all time with no filters needs no extra request', () => {
  assert.equal(usageEmptyKind('all', {}, null), 'none-yet')
  assert.equal(needsAnyEventCheck('all', {}), false)
})

test('empty Usage page: a customer, meter or dimension filter always reads as no match', () => {
  assert.equal(usageEmptyKind('all', { meter: 'mtr_1' }, false), 'no-match')
  assert.equal(usageEmptyKind('30d', { dim: 'model=gpt-4' }, false), 'no-match')
  assert.equal(usageEmptyKind('30d', { customer: 'cus_1' }, false), 'no-match')
  assert.equal(needsAnyEventCheck('30d', { customer: 'cus_1' }), false)
})

test('export window: the page window when it fits in 365 days', () => {
  assert.deepEqual(usageExportWindow({ from: '2026-09-16', to: '' }, '2026-10-16'), { from: '2026-09-16', to: '2026-10-16', clamped: false })
  assert.deepEqual(usageExportWindow({ from: '2026-01-01', to: '2026-03-31' }, '2026-10-16'), { from: '2026-01-01', to: '2026-03-31', clamped: false })
})

test('export window: All time and longer windows keep the last 365 days and say so', () => {
  assert.deepEqual(usageExportWindow({ from: '', to: '' }, '2026-10-16'), { from: '2025-10-17', to: '2026-10-16', clamped: true })
  assert.deepEqual(usageExportWindow({ from: '2020-01-01', to: '2024-02-29' }, '2026-10-16'), { from: '2023-03-02', to: '2024-02-29', clamped: true })
})

// The server refuses an export longer than 366 x 24 hours. On 2026-11-01 a
// New York tenant's last year of days holds two autumn clock changes
// (2025-11-02 and 2026-11-01) and one spring change, so it is an hour longer
// than its day count. The window must still fit.
test('export window: the bounds sent fit the server limit across clock changes', () => {
  const tz = 'America/New_York'
  const win = usageExportWindow({ from: '', to: '' }, '2026-11-01')
  const span = Date.parse(endOfDayInTZ(win.to, tz)) - Date.parse(startOfDayInTZ(win.from, tz))
  assert.ok(span <= 366 * 24 * 3600 * 1000, `span ${span / 3600000}h is over 366 days`)
})

test('dimension filter: key=value pairs pass, half-typed input is refused', () => {
  assert.deepEqual(parseDimensionFilter(''), { ok: true, value: '' })
  assert.deepEqual(parseDimensionFilter(' model = gpt-4o , token_type=input '), { ok: true, value: 'model=gpt-4o,token_type=input' })
  assert.equal(parseDimensionFilter('model').ok, false)
  assert.equal(parseDimensionFilter('model=').ok, false)
  assert.equal(parseDimensionFilter('=gpt').ok, false)
  assert.equal(parseDimensionFilter('model=a,token_type').ok, false)
})

// Backs MANUAL_TEST FLOW TZ1 (date filters on a cold load): the bounds the
// page sends are a function of the tenant zone. The page memoises them on that
// zone, so when /v1/settings arrives after the first render they are rebuilt.
import { windowQueryBounds } from '../src/lib/usageRange.ts'

test('windowQueryBounds: last 30 days for a New York tenant starts at New York midnight', () => {
  // Oct 16 2026 22:00 EDT = Oct 17 02:00Z. The UTC civil day is already Oct 17.
  const now = new Date('2026-10-17T02:00:00Z')
  const ny = windowQueryBounds('30d', noCustom, 'America/New_York', now)
  assert.equal(ny.window.from, '2026-09-16')
  assert.equal(ny.from, '2026-09-16T04:00:00.000Z')
  assert.equal(ny.to, '')
  // The pre-settings 'UTC' fallback gives a different window, which is why the
  // page must recompute when the zone loads.
  const utc = windowQueryBounds('30d', noCustom, 'UTC', now)
  assert.equal(utc.from, '2026-09-17T00:00:00.000Z')
})

test('windowQueryBounds: custom dates become start and end of day in the given zone', () => {
  const b = windowQueryBounds('custom', { from: '2026-05-05', to: '2026-05-06' }, 'Asia/Kolkata')
  assert.equal(b.from, '2026-05-04T18:30:00.000Z')
  assert.equal(b.to, '2026-05-06T18:29:59.999Z')
  assert.deepEqual(windowQueryBounds('all', noCustom, 'Asia/Kolkata'), { window: noCustom, from: '', to: '' })
})
