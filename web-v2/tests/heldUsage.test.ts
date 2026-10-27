// Held usage notice (#1044): one line per external ID that says what is held
// and why, and a release summary that says what Velox found for the rows and
// promises nothing about the invoice.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { canRelease, groupHeldByCustomer, heldLine, heldNextStep, heldUnlisted, releaseSummary } from '../src/lib/heldUsage.ts'
import type { HeldUsageGroup } from '../src/lib/gen/schemas/heldUsageGroup.ts'

const g = (over: Partial<HeldUsageGroup>): HeldUsageGroup => ({
  external_customer_id: 'acme',
  event_name: 'tokens',
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
    g({ event_name: 'calls', rows: 2, meter_exists: false, first_event_at: '2026-10-01T00:00:00Z' }),
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

// in_open_period counts rows after the period end that are not checked
// against prices, so the line names the period and promises no charge.
test('the release summary does not promise rows in a period not invoiced yet are charged', () => {
  const s = releaseSummary({ customer_id: 'c', released: 2, deduplicated: 0, still_held: 0, more_to_release: 0, in_open_period: 2, no_price: 0, not_on_plan: 0, pending_threshold_invoice: 0, outside_open_period: 0 })
  assert.equal(s.description, '2 fall in a billing period not invoiced yet.')
  assert.doesNotMatch(s.description, /will be invoiced|charges them/)
})

// The counts are what Velox found at the release; the toast says that and
// makes no promise about any invoice.
test('the release summary says what Velox found for rows no subscription bills', () => {
  const s = releaseSummary({ customer_id: 'c', released: 3, deduplicated: 1, still_held: 0, more_to_release: 0, in_open_period: 1, no_price: 0, not_on_plan: 0, pending_threshold_invoice: 0, outside_open_period: 2 })
  assert.equal(s.title, 'Released 4 usage rows')
  assert.match(s.description, /1 fall in a billing period not invoiced yet\./)
  // These rows include trial time and time after a cancel inside the
  // current period (review round 2, #1390).
  assert.match(s.description, /2 fall in time no subscription bills \(before it starts, in a free trial, or after it ends\) or in time already invoiced\. Bill them with a one-off invoice if needed\./)
  assert.doesNotMatch(s.description, /no subscription invoice includes|no invoice charges/)
  assert.doesNotMatch(s.description, /current billing period|not built yet/)
  assert.match(s.description, /1 were already recorded/)

  const none = releaseSummary({ customer_id: 'c', released: 0, deduplicated: 0, still_held: 2, more_to_release: 0, in_open_period: 0, no_price: 0, not_on_plan: 0, pending_threshold_invoice: 0, outside_open_period: 0 })
  assert.equal(none.title, 'Nothing released')
  assert.match(none.description, /2 usage rows still held/)
  assert.doesNotMatch(none.description, /invoiced/)
})

// Review findings (#1151): a meter the plan does not price is never called
// invoiced, and rows left by the 5,000-row cap are not blamed on a missing
// meter.
test('the release summary names unpriced rows and rows past the cap apart', () => {
  const s = releaseSummary({ customer_id: 'c', released: 5000, deduplicated: 0, still_held: 0, more_to_release: 1000, in_open_period: 0, no_price: 0, not_on_plan: 5000, pending_threshold_invoice: 0, outside_open_period: 0 })
  assert.match(s.description, /5,000 are for a meter the subscription's plan has no price for\./)
  assert.doesNotMatch(s.description, /will be invoiced|no invoice charges/)
  assert.match(s.description, /1,000 usage rows more are ready.*release again/)
  assert.doesNotMatch(s.description, /a meter is missing/)
})

// Design G9 (D08 PR-6): rows no price of the plan takes are never called
// invoiced.
test('the release summary names rows no price matches', () => {
  const s = releaseSummary({ customer_id: 'c', released: 3, deduplicated: 0, still_held: 0, more_to_release: 0, in_open_period: 1, no_price: 2, not_on_plan: 0, pending_threshold_invoice: 0, outside_open_period: 0 })
  assert.match(s.description, /1 fall in a billing period not invoiced yet\./)
  assert.match(s.description, /2 fall in a billing period not invoiced yet, but no price on the subscription's plan matches them\./)
  assert.doesNotMatch(s.description, /no invoice charges/)
  assert.doesNotMatch(s.description, /for a meter/)
})

// Review finding 3 (#1350): rows in time an unpaid spend-threshold invoice
// covered are not called invoiced. The line says when the period's invoice
// can charge them, and does not say a write-off settles it (#1390 review
// round 5: a written-off invoice can still be voided).
test('the release summary says rows under an unpaid threshold invoice may or may not be billed', () => {
  const s = releaseSummary({ customer_id: 'c', released: 4, deduplicated: 0, still_held: 0, more_to_release: 0, in_open_period: 0, no_price: 0, not_on_plan: 0, pending_threshold_invoice: 4, outside_open_period: 0 })
  assert.match(s.description, /4 fall in time this period's spend-threshold invoice covered, and that invoice is not paid yet/)
  assert.match(s.description, /The period's invoice charges them only if the spend-threshold invoice is voided before the period's invoice is built\. A written-off invoice can still be voided\./)
  assert.doesNotMatch(s.description, /no invoice charges/)
  assert.doesNotMatch(s.description, /will be invoiced/)
})

test('the notice says when the list is cut', () => {
  const listed = groupHeldByCustomer([g({ rows: 2 }), g({ external_customer_id: 'beta', rows: 3 })])
  assert.equal(heldUnlisted(listed, 2, 5), '')
  assert.equal(
    heldUnlisted(listed, 152, 305),
    'Showing the 2 customers held most recently. 300 usage rows for 150 more customers are held too; release or discard these to see them.',
  )
})
