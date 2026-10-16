// The Activity card's Cycle header shows the same inclusive period as the
// Upcoming card above it (current_billing_period_display), not the exclusive
// end boundary. Explicit rolling windows are left to the caller.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { activityCyclePeriodLabel } from '../src/lib/activityPeriod.ts'

test('Cycle preset reads Oct 1 – Oct 31, not → Nov 1', () => {
  const period = { from: '2026-10-01T00:00:00Z', to: '2026-11-01T00:00:00Z', source: 'current_billing_cycle' }
  assert.equal(activityCyclePeriodLabel(period, 'UTC'), 'Oct 1, 2026 – Oct 31, 2026')
})

test('Cycle preset is read in the given (org) timezone', () => {
  // New York period Oct 1 00:00 EDT → Nov 1 00:00 EDT.
  const period = { from: '2026-10-01T04:00:00Z', to: '2026-11-01T04:00:00Z', source: 'current_billing_cycle' }
  assert.equal(activityCyclePeriodLabel(period, 'America/New_York'), 'Oct 1, 2026 – Oct 31, 2026')
})

test('explicit windows are not rewritten', () => {
  const period = { from: '2026-09-16T12:00:00Z', to: '2026-10-16T12:00:00.001Z', source: 'explicit' }
  assert.equal(activityCyclePeriodLabel(period, 'UTC'), null)
})
