// Held usage notice (#1044): one line per external ID that says what is held
// and why, and a release summary that never claims rows will be invoiced
// when they fall outside the current billing period.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { canRelease, groupHeldByCustomer, heldLine, heldNextStep, releaseSummary } from '../src/lib/heldUsage.ts'
import type { HeldUsageGroup } from '../src/lib/gen/schemas/heldUsageGroup.ts'

const g = (over: Partial<HeldUsageGroup>): HeldUsageGroup => ({
  external_customer_id: 'acme',
  meter_key: 'tokens',
  reason: 'customer_not_found',
  rows: 3,
  total_quantity: '300',
  first_event_at: '2026-10-02T00:00:00Z',
  last_event_at: '2026-10-03T00:00:00Z',
  last_received_at: '2026-10-03T00:00:00Z',
  customer_id: '',
  meter_exists: true,
  ...over,
})

test('groups merge per external ID and keep the widest time range', () => {
  const out = groupHeldByCustomer([
    g({}),
    g({ meter_key: 'calls', rows: 2, meter_exists: false, first_event_at: '2026-10-01T00:00:00Z' }),
    g({ external_customer_id: 'beta', rows: 1 }),
  ])
  assert.equal(out.length, 2)
  assert.equal(out[0].rows, 5)
  assert.deepEqual(out[0].missingMeters, ['calls'])
  assert.equal(out[0].firstEventAt, '2026-10-01T00:00:00Z')
})

test('the line names the missing customer, or the missing meter', () => {
  const [missingCustomer] = groupHeldByCustomer([g({ rows: 1 })])
  assert.equal(heldLine(missingCustomer), "1 usage row held: customer 'acme' does not exist yet")
  assert.match(heldNextStep(missingCustomer), /Create a customer with external ID 'acme'/)

  const [missingMeter] = groupHeldByCustomer([g({ customer_id: 'vlx_cus_1', meter_exists: false, reason: 'meter_not_found' })])
  assert.equal(heldLine(missingMeter), "3 usage rows held for customer 'acme': meter 'tokens' does not exist yet")
  assert.match(heldNextStep(missingMeter), /missing meter/)
})

test('release is offered only once the customer and a meter exist', () => {
  const noCustomer = [g({})]
  assert.equal(canRelease(groupHeldByCustomer(noCustomer)[0], noCustomer), false)
  const noMeter = [g({ customer_id: 'vlx_cus_1', meter_exists: false })]
  assert.equal(canRelease(groupHeldByCustomer(noMeter)[0], noMeter), false)
  const ready = [g({ customer_id: 'vlx_cus_1' })]
  assert.equal(canRelease(groupHeldByCustomer(ready)[0], ready), true)
})

test('the release summary says which rows a subscription invoice will not include', () => {
  const s = releaseSummary({ customer_id: 'c', released: 3, deduplicated: 1, still_held: 0, in_open_period: 1, outside_open_period: 2 })
  assert.equal(s.title, 'Released 4 usage rows')
  assert.match(s.description, /1 fall in the current billing period/)
  assert.match(s.description, /2 fall outside the current billing period.*one-off invoice/)
  assert.match(s.description, /1 were already recorded/)

  const none = releaseSummary({ customer_id: 'c', released: 0, deduplicated: 0, still_held: 2, in_open_period: 0, outside_open_period: 0 })
  assert.equal(none.title, 'Nothing released')
  assert.match(none.description, /2 usage rows still held/)
  assert.doesNotMatch(none.description, /invoiced/)
})
