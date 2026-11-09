// Pins the days the date picker offers. A credit's "Valid through" day is a
// day of the account's zone, sent as the next midnight there. The picker used
// the browser's zone and wall clock instead: with the browser behind the
// account's zone, it offered a day that had already ended there, which the
// server refused, and on a test clock its Today button picked the real date.
//
// This file runs with the browser's zone set to Los Angeles, so a day taken
// from the browser would differ from the account's.
import { afterAll, test } from 'vitest'
import assert from 'node:assert/strict'
import { format } from 'date-fns'
import { effectiveNow } from '../src/lib/effectiveNow.ts'
import { dayAsDate, pickerDays } from '../src/lib/datePickerDays.ts'

const browserZone = process.env.TZ
process.env.TZ = 'America/Los_Angeles'
afterAll(() => {
  if (browserZone === undefined) delete process.env.TZ
  else process.env.TZ = browserZone
})

test('with the browser in Los Angeles and the account in Tokyo, today is the Tokyo day', () => {
  // 20:00 on Nov 8 in Los Angeles is 13:00 on Nov 9 in Tokyo.
  const now = effectiveNow('2026-11-09T04:00:00Z')
  assert.equal(format(new Date(now), 'yyyy-MM-dd'), '2026-11-08', 'the browser reads Nov 8')
  assert.equal(pickerDays(now, 'Asia/Tokyo').today, '2026-11-09')
  assert.equal(pickerDays(now, 'America/Los_Angeles').today, '2026-11-08')
})

test("on a test clock, today is the clock's day in the account zone", () => {
  // A clock frozen at 20:00 UTC on Jan 15: Jan 16 in Tokyo, whatever the
  // real date is.
  const now = effectiveNow('2026-01-15T20:00:00Z')
  assert.equal(pickerDays(now, 'Asia/Tokyo').today, '2026-01-16')
  assert.equal(pickerDays(now, 'UTC').today, '2026-01-15')
})

test('Today can be picked only on or after the first day allowed', () => {
  const now = effectiveNow('2026-01-15T20:00:00Z')
  assert.equal(pickerDays(now, 'Asia/Tokyo').todayAllowed, true, 'no first day')
  assert.equal(pickerDays(now, 'Asia/Tokyo', '2026-01-16').todayAllowed, true, 'today is the first day')
  assert.equal(pickerDays(now, 'Asia/Tokyo', '2026-01-17').todayAllowed, false, 'the first day is tomorrow')
})

test('a day drawn in the calendar is the same day in the browser zone', () => {
  // The calendar draws a day as its local midnight. On Mar 8 Los Angeles
  // moves its clocks, and the day must still read back as itself.
  for (const day of ['2026-01-16', '2026-03-08', '2026-11-01', '2026-12-31']) {
    const drawn = dayAsDate(day)
    assert.equal(format(drawn, 'yyyy-MM-dd'), day)
  }
})
