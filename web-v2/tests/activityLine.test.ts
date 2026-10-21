// Pins how the dashboard shows an activity line (ADR-128): the parts it
// formats (money, time, link), the four tones, the actor words, the day
// headers and the zone header. The backend builds every word; these checks
// cover only what the dashboard adds.
import { test, beforeEach } from 'vitest'
import assert from 'node:assert/strict'
import {
  TONES, actorText, dayKey, groupByDay, linkRoute, money, partTime, recordedTime, timeOfDay, toneOf, zoneHeader,
} from '../src/lib/activityLine.ts'
import { setActiveCurrency } from '../src/lib/money.ts'
import type { ActivityLine } from '../src/lib/gen/schemas/activityLine.ts'

const KOLKATA = 'Asia/Kolkata'
const NEW_YORK = 'America/New_York'

function line(id: string, occurredAt: string): ActivityLine {
  return { id, kind: 'invoice.finalized', title: 'Invoice finalized', detail: [], tone: 'neutral', occurred_at: occurredAt }
}

beforeEach(() => setActiveCurrency('USD'))

test('money is shown in its own currency, never the workspace default', () => {
  assert.equal(money({ cents: 123450, currency: 'USD' }), '$1,234.50')
  assert.equal(money({ cents: 1200, currency: 'CAD' }), 'CA$12.00')
  // Negative control: a CAD amount must not read as the USD default.
  assert.notEqual(money({ cents: 1200, currency: 'CAD' }), '$12.00')
  assert.equal(money({ cents: -500, currency: 'EUR' }), '-€5.00')
})

test('a row time is the time of day in the account zone', () => {
  // 04:30 UTC is 10:00 AM in Kolkata (GMT+5:30), the e2e frozen clock.
  assert.equal(timeOfDay('2027-02-01T04:30:00Z', KOLKATA), '10:00 AM')
  assert.equal(timeOfDay('2027-02-01T04:30:00Z', 'UTC'), '4:30 AM')
})

test('a time part carries its date and time, with no zone (the header names it)', () => {
  assert.equal(partTime('2026-11-01T03:30:00Z', KOLKATA), 'Nov 1, 2026, 9:00 AM')
})

test('the Recorded part shows the year only when it is not the current year', () => {
  const now = Date.parse('2026-10-21T06:00:00Z')
  assert.equal(recordedTime('2026-10-18T14:44:00Z', KOLKATA, now), 'Oct 18, 8:14 PM')
  // 20:00 UTC on Dec 31, 2025 is already 2026 in Kolkata: the year is the
  // account zone's, so it is the current year and not shown.
  assert.equal(recordedTime('2025-12-31T20:00:00Z', KOLKATA, now), 'Jan 1, 1:30 AM')
  assert.equal(recordedTime('2025-12-31T17:00:00Z', KOLKATA, now), 'Dec 31, 2025, 10:30 PM')
})

test('a link goes to the page of the object it names', () => {
  assert.equal(linkRoute({ kind: 'invoice', id: 'vlx_inv_1', label: 'VLX-000123' }), '/invoices/vlx_inv_1')
  assert.equal(linkRoute({ kind: 'subscription', id: 'vlx_sub_1', label: 'Starter' }), '/subscriptions/vlx_sub_1')
  // A credit note has no page of its own: the list, filtered to its number.
  assert.equal(linkRoute({ kind: 'credit_note', id: 'vlx_cn_1', label: 'CN-000012' }), '/credit-notes?search=CN-000012')
  // An unknown kind from a newer server draws no link.
  assert.equal(linkRoute({ kind: 'plan' as never, id: 'x', label: 'x' }), null)
})

test('four tones, one colour each; green only for success', () => {
  assert.deepEqual(Object.keys(TONES).sort(), ['failure', 'neutral', 'success', 'warning'])
  assert.equal(TONES.neutral.dot, 'bg-muted-foreground')
  assert.equal(TONES.success.dot, 'bg-success')
  assert.equal(TONES.warning.dot, 'bg-warning')
  assert.equal(TONES.failure.dot, 'bg-destructive')
  assert.equal(TONES.warning.problem, 'text-warning-foreground')
  assert.equal(TONES.failure.problem, 'text-destructive')
  // Only the success tone is green.
  for (const [tone, cls] of Object.entries(TONES)) {
    assert.equal(cls.dot.includes('success'), tone === 'success', tone)
  }
  // An unknown tone reads as neutral, so the row still has a dot.
  assert.equal(toneOf('escalated'), 'neutral')
  assert.equal(toneOf(undefined), 'neutral')
  assert.equal(toneOf('failure'), 'failure')
})

test('the actor reads from the fixed set, the same words as the backend', () => {
  assert.equal(actorText({ kind: 'person', name: 'sagar@example.com' }), 'by sagar@example.com')
  assert.equal(actorText({ kind: 'api_key', name: 'billing-sync' }), 'by API key billing-sync')
  assert.equal(actorText({ kind: 'api_key' }), 'by API key')
  assert.equal(actorText({ kind: 'customer', via: 'payment_page' }), 'Customer, on the payment page')
  assert.equal(actorText({ kind: 'customer', via: 'portal' }), 'Customer, in the portal')
  assert.equal(actorText({ kind: 'automatic' }), 'Automatic')
  assert.equal(actorText(undefined), '')
})

test('rows group under day headers on the account calendar, in the server order', () => {
  // 20:00 UTC on Jan 31 is already Feb 1 in Kolkata.
  const days = groupByDay([
    line('a', '2027-01-31T20:00:00Z'),
    line('b', '2027-02-01T04:30:00Z'),
    line('c', '2027-02-02T04:30:00Z'),
  ], KOLKATA)
  assert.deepEqual(days.map(d => [d.label, d.lines.map(l => l.id)]), [
    ['Feb 1, 2027', ['a', 'b']],
    ['Feb 2, 2027', ['c']],
  ])
  // In UTC the same rows fall on three days.
  assert.equal(groupByDay([line('a', '2027-01-31T20:00:00Z'), line('b', '2027-02-01T04:30:00Z')], 'UTC').length, 2)
  assert.equal(dayKey('2027-01-31T20:00:00Z', KOLKATA), '2027-02-01')
})

test('grouping never reorders: a day that comes back gets its header again', () => {
  const days = groupByDay([
    line('a', '2027-02-02T04:30:00Z'),
    line('b', '2027-02-01T04:30:00Z'),
    line('c', '2027-02-02T05:30:00Z'),
  ], KOLKATA)
  assert.deepEqual(days.map(d => d.lines.map(l => l.id)), [['a'], ['b'], ['c']])
})

test('the zone is named once, as the dashboard formatter names it', () => {
  assert.equal(zoneHeader(['2027-02-01T04:30:00Z', '2027-02-02T04:30:00Z'], KOLKATA), 'Times in GMT+5:30')
  // Rows on both sides of a summer-time change name both.
  assert.equal(zoneHeader(['2027-02-01T15:00:00Z', '2027-04-01T15:00:00Z'], NEW_YORK), 'Times in EST / EDT')
  assert.equal(zoneHeader([], KOLKATA), '')
})
