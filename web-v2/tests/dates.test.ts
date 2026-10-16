// Locks MANUAL_TEST FLOW TZ1.3: civil-date pickers/filters interpret the picked
// yyyy-mm-dd as start/end of that day IN THE TENANT TIMEZONE, returning the right
// UTC instant — so a "from May 5" filter or an "expires May 5" key means the
// operator's civil day regardless of who's viewing. Pure functions; runs on the
// built-in runner (the @/lib/api import is stubbed via tests/support).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { startOfDayInTZ, endOfDayInTZ, isAfterCivilToday, formatInvoiceDate } from '../src/lib/dates.ts'

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
