// One set of subscription dates for the summary, the timeline, the list and
// the cancel dialog (M-SUB-3, M-SUB-4, M-SUB-5).
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { cancelAtPeriodEndText, nextBillCell, periodLabels } from '../src/lib/subscriptionPeriod.ts'

// UTC renders: the event day, and the day before a midnight boundary.
const fmt = {
  date: (iso: string) => iso.slice(0, 10),
  lastDay: (iso: string) => new Date(new Date(iso).getTime() - 1).toISOString().slice(0, 10),
}

const active = {
  status: 'active' as const,
  current_billing_period_start: '2026-10-16T00:00:00Z',
  current_billing_period_end: '2026-11-16T00:00:00Z',
  next_billing_at: '2026-11-16T00:00:00Z',
}

test('an active subscription renews on the boundary and has access through the day before', () => {
  const l = periodLabels(active, fmt)
  assert.equal(l.renewsOn, '2026-11-16')
  assert.equal(l.accessThrough, '2026-11-15')
  assert.deepEqual(l.next, { label: 'Renews', value: '2026-11-16', tone: 'normal' })
  assert.deepEqual(nextBillCell(l), { text: 'Renews 2026-11-16', tone: 'normal' })
})

test('a canceled subscription never renews', () => {
  const l = periodLabels({ ...active, status: 'canceled' as const, canceled_at: '2026-10-16T09:00:00Z' }, fmt)
  assert.equal(l.renewsOn, null)
  assert.equal(l.accessThrough, null)
  assert.deepEqual(l.next, { label: 'Canceled', value: '2026-10-16', tone: 'ended' })
  assert.deepEqual(nextBillCell(l), { text: 'Ended 2026-10-16', tone: 'ended' })
})

test('a scheduled cancel replaces the renewal and names both days', () => {
  const l = periodLabels({ ...active, cancel_at_period_end: true, cancel_effective_at: '2026-11-16T00:00:00Z' }, fmt)
  assert.equal(l.renewsOn, null)
  assert.equal(l.cancelsOn, '2026-11-16')
  assert.deepEqual(l.next, { label: 'Cancels', value: '2026-11-16', tone: 'scheduled' })
  assert.equal(
    cancelAtPeriodEndText(l),
    "The customer keeps access through 2026-11-15 and the subscription doesn't renew on 2026-11-16. You can undo this until then.",
  )
})

test('the cancel dialog on an active subscription names the renewal that stops', () => {
  assert.equal(
    cancelAtPeriodEndText(periodLabels(active, fmt)),
    "The customer keeps access through 2026-11-15 and the subscription doesn't renew on 2026-11-16. You can undo this until then.",
  )
})

