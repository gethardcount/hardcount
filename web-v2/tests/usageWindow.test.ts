// Windows for the customer page's Activity and Margin cards (ADR-121).
// Pure functions, no DOM: runs with `npm test`.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { effectiveNow } from '../src/lib/effectiveNow.ts'
import {
  DEFAULT_ACTIVITY_PRESET,
  DEFAULT_MARGIN_PRESET,
  MARGIN_PRESETS,
  isOutsideWindow,
  lastInvoicedPeriod,
  marginWindowFor,
  marginWindowIsEmpty,
  rangeIncluding,
  usageWindowIsEmpty,
} from '../src/lib/usageWindow.ts'

// The demo scenario: a clock starts 2026-01-31T10:00Z, usage is sent at that
// instant, the clock is advanced 32 days to 2026-03-04T10:00Z. The first
// period [Jan 31, Feb 28) is invoiced; the current cycle is [Feb 28, Mar 31).
const usageAt = '2026-01-31T10:00:00Z'
const clock = effectiveNow('2026-03-04T10:00:00Z')
const cycle = { from: '2026-02-28T00:00:00Z', to: '2026-03-31T00:00:00Z' }
const invoices = [
  {
    status: 'finalized',
    subscription_id: 'sub_1',
    billing_period_start: '2026-01-31T00:00:00Z',
    billing_period_end: '2026-02-28T00:00:00Z',
  },
]
const base = { cycle, lastInvoiced: lastInvoicedPeriod(invoices), now: clock, customFrom: '', customTo: '', timezone: 'UTC' }

test('both cards open on the current billing period', () => {
  assert.equal(DEFAULT_MARGIN_PRESET, 'cycle')
  assert.equal(DEFAULT_ACTIVITY_PRESET, 'cycle')
  assert.equal(MARGIN_PRESETS[0].key, 'cycle')
  assert.equal(MARGIN_PRESETS[0].label, 'Current period')
  assert.deepEqual(marginWindowFor('cycle', base), { kind: 'ready', ...cycle, period: true })
})

test('demo: the last invoiced period holds the usage the invoice billed', () => {
  const w = marginWindowFor('last_invoiced', base)
  assert.equal(w.kind, 'ready')
  if (w.kind !== 'ready') return
  assert.equal(isOutsideWindow(usageAt, w.from, w.to), false)
  // The rolling 30 days this replaces as the default starts two days after it.
  const rolling = marginWindowFor('30d', base)
  assert.equal(rolling.kind, 'ready')
  if (rolling.kind !== 'ready') return
  assert.equal(isOutsideWindow(usageAt, rolling.from, rolling.to), true)
  assert.equal(rolling.period, false)
})

test('marginWindowFor waits for the period, and names a missing one', () => {
  assert.deepEqual(marginWindowFor('cycle', { ...base, cycle: undefined }), { kind: 'waiting' })
  assert.deepEqual(marginWindowFor('cycle', { ...base, cycle: null }), { kind: 'no-cycle' })
  assert.deepEqual(marginWindowFor('last_invoiced', { ...base, lastInvoiced: undefined }), { kind: 'waiting' })
  assert.deepEqual(marginWindowFor('last_invoiced', { ...base, lastInvoiced: null }), { kind: 'no-invoice' })
  assert.deepEqual(marginWindowFor('custom', { ...base, customFrom: '2026-01-01' }), { kind: 'pick-dates' })
})

test('a custom range runs from start of day to end of day in the tenant zone', () => {
  const w = marginWindowFor('custom', { ...base, customFrom: '2026-01-31', customTo: '2026-01-31', timezone: 'America/New_York' })
  assert.deepEqual(w, { kind: 'ready', from: '2026-01-31T05:00:00.000Z', to: '2026-02-01T04:59:59.999Z', period: false })
})

test('lastInvoicedPeriod takes the latest period end and skips drafts, voids and one-offs', () => {
  const rows = [
    { status: 'paid', subscription_id: 's', billing_period_start: '2026-01-01T00:00:00Z', billing_period_end: '2026-02-01T00:00:00Z' },
    { status: 'draft', subscription_id: 's', billing_period_start: '2026-03-01T00:00:00Z', billing_period_end: '2026-04-01T00:00:00Z' },
    { status: 'voided', subscription_id: 's', billing_period_start: '2026-02-15T00:00:00Z', billing_period_end: '2026-03-15T00:00:00Z' },
    { status: 'finalized', subscription_id: '', billing_period_start: '2026-02-20T00:00:00Z', billing_period_end: '2026-03-20T00:00:00Z' },
    { status: 'uncollectible', subscription_id: 's', billing_period_start: '2026-02-01T00:00:00Z', billing_period_end: '2026-03-01T00:00:00Z' },
  ]
  assert.deepEqual(lastInvoicedPeriod(rows), { from: '2026-02-01T00:00:00Z', to: '2026-03-01T00:00:00Z' })
  assert.equal(lastInvoicedPeriod([]), null)
  assert.equal(lastInvoicedPeriod([rows[1], rows[2]]), null)
})

test('isOutsideWindow uses [from, to): the end instant is outside', () => {
  assert.equal(isOutsideWindow(cycle.from, cycle.from, cycle.to), false)
  assert.equal(isOutsideWindow(cycle.to, cycle.from, cycle.to), true)
})

test('rangeIncluding runs from the latest usage day to today, in the tenant zone', () => {
  assert.deepEqual(rangeIncluding(usageAt, clock, 'UTC'), { from: '2026-01-31', to: '2026-03-04' })
  // 02:00Z on Feb 1 is still Jan 31 in New York.
  assert.deepEqual(rangeIncluding('2026-02-01T02:00:00Z', clock, 'America/New_York'), { from: '2026-01-31', to: '2026-03-04' })
  // Usage after "now" still gets a range that covers it.
  assert.deepEqual(rangeIncluding('2026-04-01T00:00:00Z', clock, 'UTC'), { from: '2026-04-01', to: '2026-04-01' })
})

test('empty windows: margin with nothing in it, usage buckets all zero', () => {
  assert.equal(marginWindowIsEmpty({ revenue_cents: 0, cost_micros: 0, unresolved_events: 0, by_model: [] }), true)
  assert.equal(marginWindowIsEmpty({ revenue_cents: 0, cost_micros: 5, unresolved_events: 0, by_model: [{}] }), false)
  assert.equal(marginWindowIsEmpty({ revenue_cents: 0, cost_micros: 0, unresolved_events: 2, by_model: [] }), false)
  assert.equal(usageWindowIsEmpty([{ per_meter: {} }, { per_meter: { m: '0.000000000000' } }]), true)
  assert.equal(usageWindowIsEmpty([{ per_meter: { m: '0' } }, { per_meter: { m: '3' } }]), false)
  assert.equal(usageWindowIsEmpty([]), true)
})
