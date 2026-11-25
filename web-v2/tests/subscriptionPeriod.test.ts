// One set of subscription dates for the summary, the timeline and the list
// (M-SUB-3, M-SUB-4, M-SUB-5).
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { nextBillCell, periodLabels } from '../src/lib/subscriptionPeriod.ts'

// A UTC render of the event day.
const date = (iso: string) => iso.slice(0, 10)

const active = {
  status: 'active' as const,
  current_billing_period_start: '2026-10-16T00:00:00Z',
  current_billing_period_end: '2026-11-16T00:00:00Z',
  next_billing_at: '2026-11-16T00:00:00Z',
}

test('an active subscription renews on the boundary', () => {
  const l = periodLabels(active, date)
  assert.equal(l.renewsOn, '2026-11-16')
  assert.deepEqual(l.next, { label: 'Renews', value: '2026-11-16', tone: 'normal' })
  assert.equal(nextBillCell(l), 'Renews 2026-11-16')
})

test('a canceled subscription never renews', () => {
  const l = periodLabels({ ...active, status: 'canceled' as const, canceled_at: '2026-10-16T09:00:00Z' }, date)
  assert.equal(l.renewsOn, null)
  assert.deepEqual(l.next, { label: 'Canceled', value: '2026-10-16', tone: 'ended' })
  assert.equal(nextBillCell(l), 'Ended 2026-10-16')
})
