// Locks MANUAL_TEST FLOW TZ1.3: civil-date pickers/filters interpret the picked
// yyyy-mm-dd as start/end of that day IN THE TENANT TIMEZONE, returning the right
// UTC instant — so a "from May 5" filter or an "expires May 5" key means the
// operator's civil day regardless of who's viewing. Pure functions; runs under
// `npm test` (Vitest).
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { startOfDayInTZ, endOfDayInTZ, isAfterCivilToday, formatInvoiceDate, usageFilterEndYMD, civilDateFrom, addCalendarMonthInTZ } from '../src/lib/dates.ts'

test('startOfDayInTZ = 00:00 of the civil day in the given zone, as a UTC instant', () => {
  // 2026-05-05 00:00 IST (UTC+5:30) = the previous UTC evening.
  assert.equal(startOfDayInTZ('2026-05-05', 'Asia/Kolkata'), '2026-05-04T18:30:00.000Z')
  // 2026-05-05 00:00 PDT (May → UTC-7) = same-day 07:00 UTC.
  assert.equal(startOfDayInTZ('2026-05-05', 'America/Los_Angeles'), '2026-05-05T07:00:00.000Z')
  // UTC is the identity case.
  assert.equal(startOfDayInTZ('2026-05-05', 'UTC'), '2026-05-05T00:00:00.000Z')
})

test('endOfDayInTZ = 23:59:59.999 of the civil day in the given zone, as a UTC instant', () => {
  assert.equal(endOfDayInTZ('2026-05-05', 'Asia/Kolkata'), '2026-05-05T18:29:59.999Z')
  // 23:59:59.999 PDT rolls into the next UTC day.
  assert.equal(endOfDayInTZ('2026-05-05', 'America/Los_Angeles'), '2026-05-06T06:59:59.999Z')
  assert.equal(endOfDayInTZ('2026-05-05', 'UTC'), '2026-05-05T23:59:59.999Z')
})

test('the from/to pair brackets a row that is "May 5" in the tenant zone but not in UTC', () => {
  // 2026-05-05T02:00:00Z is 07:30 IST on May 5 — squarely inside an IST "May 5"
  // filter, though a naive UTC-day bracket [May 5 00:00Z, May 5 23:59Z] would also
  // catch it. The real trap is the boundary: this instant is BEFORE the IST-day
  // start expressed in UTC... no — it must fall within the IST bracket.
  const from = startOfDayInTZ('2026-05-05', 'Asia/Kolkata') // 2026-05-04T18:30:00.000Z
  const to = endOfDayInTZ('2026-05-05', 'Asia/Kolkata') //   2026-05-05T18:29:59.999Z
  const rowInISTMay5 = Date.parse('2026-05-05T02:00:00Z')
  assert.ok(
    Date.parse(from) <= rowInISTMay5 && rowInISTMay5 <= Date.parse(to),
    'an IST-May-5 row falls within the IST from/to bracket',
  )
  // And an instant that is May 5 in UTC but May 6 in IST (e.g. 22:00Z = 03:30 IST
  // May 6) must fall OUTSIDE the IST-May-5 bracket — the whole point of TZ anchoring.
  const rowUTCMay5ButISTMay6 = Date.parse('2026-05-05T22:00:00Z')
  assert.ok(rowUTCMay5ButISTMay6 > Date.parse(to), 'a UTC-May-5 / IST-May-6 row is excluded from the IST May-5 bracket')
})

// Locks the one-off invoice "Sells credits" expiry check: "today" is the tenant
// civil date, the same zone endOfDayInTZ resolves the expiry in.
test('isAfterCivilToday uses the tenant date, not the UTC date', () => {
  // 2026-10-16 21:00 EDT = 2026-10-17T01:00Z. Locally it is still Oct 16.
  const nowMs = Date.parse('2026-10-17T01:00:00Z')
  assert.equal(isAfterCivilToday('2026-10-17', nowMs, 'America/New_York'), true, 'tomorrow is in the future')
  assert.equal(isAfterCivilToday('2026-10-16', nowMs, 'America/New_York'), false, 'today is not')
  // East of UTC: 2026-10-16T20:00Z is Oct 17 01:30 in Kolkata, so Oct 17 is today.
  assert.equal(isAfterCivilToday('2026-10-17', Date.parse('2026-10-16T20:00:00Z'), 'Asia/Kolkata'), false)
})

// Backs MANUAL_TEST FLOW TZ1 (dashboard invoice dates): issued/due/paid/voided dates
// render in the invoice's billing timezone, the zone the PDF and hosted page
// use, not the live tenant zone. The test stub's tenant zone is unset, so the
// fallback here is 'UTC'.
test('formatInvoiceDate uses the invoice billing timezone, not the tenant zone', () => {
  // Issued under Asia/Kolkata: due 2027-07-10T18:30Z is Jul 11 there (the PDF
  // date), even if the org has since moved to America/New_York (Jul 10).
  assert.equal(formatInvoiceDate('2027-07-10T18:30:00Z', 'Asia/Kolkata'), 'Jul 11, 2027')
  assert.equal(formatInvoiceDate('2027-07-10T18:30:00Z', 'America/New_York'), 'Jul 10, 2027')
})

test('formatInvoiceDate falls back to the tenant zone when the invoice has none', () => {
  assert.equal(formatInvoiceDate('2027-07-10T18:30:00Z', ''), 'Jul 10, 2027')
  assert.equal(formatInvoiceDate('2027-07-10T18:30:00Z', undefined), 'Jul 10, 2027')
})

// Locks MANUAL_TEST FLOW I1 "drill-down covers a cancel invoice's last day":
// the invoice line → /usage link's `to` is the last day the invoice billed.
test('usageFilterEndYMD steps back a day only when the period ends at civil midnight', () => {
  // Normal cycle Oct 1 → Nov 1 (00:00 UTC): Nov 1 belongs to the next period.
  assert.equal(usageFilterEndYMD('2026-10-01T00:00:00Z', '2026-11-01T00:00:00Z', 'UTC'), '2026-10-31')
  // Same cycle in New York: the boundary is midnight EDT, not UTC.
  assert.equal(usageFilterEndYMD('2026-10-01T04:00:00Z', '2026-11-01T04:00:00Z', 'America/New_York'), '2026-10-31')
})

test('usageFilterEndYMD keeps the end day of a mid-day cancel invoice', () => {
  // Cancel at Oct 16 15:00: usage from Oct 16 00:00-15:00 was billed.
  assert.equal(usageFilterEndYMD('2026-10-01T00:00:00Z', '2026-10-16T15:00:00Z', 'UTC'), '2026-10-16')
})

test('usageFilterEndYMD never ends before the start (same-day cancel)', () => {
  // Period Oct 16 00:00 → Oct 16 14:23: from=2026-10-16 must not get to=2026-10-15.
  assert.equal(usageFilterEndYMD('2026-10-16T00:00:00Z', '2026-10-16T14:23:00Z', 'UTC'), '2026-10-16')
  // A zero-length stub at midnight still yields a non-inverted range.
  assert.equal(usageFilterEndYMD('2026-10-16T00:00:00Z', '2026-10-16T00:00:00Z', 'UTC'), '2026-10-16')
})

// Locks the pause dialog's earliest auto-resume date: tomorrow in tenant TZ,
// from the subscription's effective now. Its start of day is always after now,
// so the server's "resumes_at must be in the future" check passes.
test('civilDateFrom(now, 1) is tomorrow in the tenant zone, and its start is in the future', () => {
  // 2026-10-16 21:00 EDT = 2026-10-17T01:00Z: tomorrow is Oct 17 locally.
  const nyEvening = Date.parse('2026-10-17T01:00:00Z')
  assert.equal(civilDateFrom(nyEvening, 1, 'America/New_York'), '2026-10-17')
  assert.ok(Date.parse(startOfDayInTZ(civilDateFrom(nyEvening, 1, 'America/New_York'), 'America/New_York')) > nyEvening)
  // Mid-morning UTC: today's start is already past, tomorrow's is not.
  const utcMorning = Date.parse('2026-10-16T09:00:00Z')
  assert.equal(civilDateFrom(utcMorning, 1, 'UTC'), '2026-10-17')
  assert.ok(Date.parse(startOfDayInTZ(civilDateFrom(utcMorning, 1, 'UTC'), 'UTC')) > utcMorning)
  // Month and year rollover.
  assert.equal(civilDateFrom(Date.parse('2026-12-31T12:00:00Z'), 1, 'UTC'), '2027-01-01')
})

// Backs MANUAL_TEST FLOW TC2 ("+1mo" preset): the month step runs on
// the tenant-zone wall clock with an end-of-month clamp, like the engine's
// advanceAnchored. Every case names its zone explicitly, so the result must not
// depend on the machine TZ — `npm test` is also run with TZ=America/Los_Angeles
// and TZ=Asia/Kolkata to prove it.
const plusMonth = (iso: string, tz: string) => addCalendarMonthInTZ(new Date(iso), tz).toISOString()

test('addCalendarMonthInTZ clamps a month-end day to the next month\'s last day', () => {
  // Jan 31 00:00 UTC → Feb 28, not Mar 1 (browser-local drift) or Mar 3 (overflow).
  assert.equal(plusMonth('2026-01-31T00:00:00Z', 'UTC'), '2026-02-28T00:00:00.000Z')
  // Leap year keeps Feb 29.
  assert.equal(plusMonth('2028-01-31T00:00:00Z', 'UTC'), '2028-02-29T00:00:00.000Z')
  // December rolls into January of the next year.
  assert.equal(plusMonth('2026-12-31T00:00:00Z', 'UTC'), '2027-01-31T00:00:00.000Z')
})

test('addCalendarMonthInTZ lands on the 1st-of-month cycle close in the tenant zone', () => {
  // Oct 1 00:00 UTC is Sep 30 in Los Angeles; stepping browser-local fields gave Oct 31.
  assert.equal(plusMonth('2026-10-01T00:00:00Z', 'UTC'), '2026-11-01T00:00:00.000Z')
  // Asia/Kolkata tenant: Jan 31 00:00 IST (= Jan 30 18:30Z) → Feb 28 00:00 IST.
  assert.equal(plusMonth('2026-01-30T18:30:00Z', 'Asia/Kolkata'), '2026-02-27T18:30:00.000Z')
})

test('addCalendarMonthInTZ keeps the tenant wall time across a DST change', () => {
  // Feb 15 09:00 EST (14:00Z) → Mar 15 09:00 EDT (13:00Z): same wall time, one hour less UTC.
  assert.equal(plusMonth('2026-02-15T14:00:00Z', 'America/New_York'), '2026-03-15T13:00:00.000Z')
  // A UTC tenant has no DST: Feb 15 12:00Z → Mar 15 12:00Z. A Los Angeles browser
  // stepping its own fields crossed its Mar 8 DST change and landed on 11:00Z.
  assert.equal(plusMonth('2026-02-15T12:00:00Z', 'UTC'), '2026-03-15T12:00:00.000Z')
})
