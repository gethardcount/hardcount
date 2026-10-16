// The invoice due badge counts calendar days in the tenant zone, so it agrees
// with the Due date printed beside it. due_at carries the issue hour
// (2026-10-16T20:16Z), which a 24h-ceil count turned into "Due in 1d" on the
// morning of the due date and "Due today" through most of the next day.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { calendarDaysUntil } from '../src/lib/dates.ts'
import { effectiveNow } from '../src/lib/effectiveNow.ts'

const due = '2026-10-16T20:16:00Z' // "Due Oct 16" in UTC

test('on the due date the count is 0 all day', () => {
  assert.equal(calendarDaysUntil(due, effectiveNow('2026-10-16T09:00:00Z'), 'UTC'), 0)
  assert.equal(calendarDaysUntil(due, effectiveNow('2026-10-16T21:00:00Z'), 'UTC'), 0)
})

test('the day after the due date is 1 day past due', () => {
  assert.equal(calendarDaysUntil(due, effectiveNow('2026-10-17T19:00:00Z'), 'UTC'), -1)
})

test('days before the due date count whole calendar days', () => {
  assert.equal(calendarDaysUntil(due, effectiveNow('2026-10-15T23:59:00Z'), 'UTC'), 1)
  assert.equal(calendarDaysUntil(due, effectiveNow('2026-09-16T01:00:00Z'), 'UTC'), 30)
})

test('dates are read in the given zone, like the printed due date', () => {
  // In New York due_at 2026-10-16T20:16Z is Oct 16 16:16; now 2026-10-17T01:00Z is Oct 16 21:00.
  assert.equal(calendarDaysUntil(due, effectiveNow('2026-10-17T01:00:00Z'), 'America/New_York'), 0)
  // Across the DST change (Nov 1) the count is still whole days.
  assert.equal(calendarDaysUntil('2026-11-02T15:00:00Z', effectiveNow('2026-10-31T15:00:00Z'), 'America/New_York'), 2)
})

test('an unparseable due date gives NaN, not a throw', () => {
  assert.ok(Number.isNaN(calendarDaysUntil('garbage', effectiveNow('2026-10-16T09:00:00Z'), 'UTC')))
})
