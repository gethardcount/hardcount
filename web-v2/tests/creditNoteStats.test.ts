// Guards the Credit Notes page's "Refunded to Card" stat.
//
// The stat used to sum refund_amount_cents over every issued CN — counting
// allocations, not outcomes — so a refund whose Stripe leg FAILED (no refund
// object even created) displayed as money returned to a card. Observed live
// 2026-08-03 on Walkthrough Co: card showed $192.60 while $30.00 of it was two
// failed legs; the true figure was $162.60. "Refunded to Card" must count only
// refund_status=succeeded; pending/failed are surfaced separately, never
// folded into the money total.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { creditNoteStats } from '../src/lib/creditNoteStats.ts'

const cn = (over: Record<string, unknown>) => ({
  status: 'issued',
  currency: 'USD',
  refund_status: 'none',
  refund_amount_cents: 0,
  credit_amount_cents: 0,
  out_of_band_amount_cents: 0,
  total_cents: 0,
  ...over,
})

// One figure per currency, as the cards print them; none when nothing.
const usd = (cents: number) => (cents ? [{ currency: 'USD', amount_cents: cents }] : [])

test('a failed refund leg is never counted as refunded to card', () => {
  const s = creditNoteStats([
    cn({ refund_amount_cents: 16260, refund_status: 'succeeded', total_cents: 16260 }),
    cn({ refund_amount_cents: 2000, refund_status: 'failed', total_cents: 2000 }),
    cn({ refund_amount_cents: 1000, refund_status: 'failed', total_cents: 1000 }),
  ])
  assert.deepEqual(s.totalRefunded, usd(16260)) // the live Walkthrough Co shape: $162.60, not $192.60
  assert.deepEqual(s.refundFailedCents, usd(3000))
  assert.deepEqual(s.refundPendingCents, usd(0))
})

test('a pending refund leg is in flight, not refunded', () => {
  const s = creditNoteStats([
    cn({ refund_amount_cents: 5000, refund_status: 'pending', total_cents: 5000 }),
  ])
  assert.deepEqual(s.totalRefunded, usd(0))
  assert.deepEqual(s.refundPendingCents, usd(5000))
})

test('draft and voided notes count toward no money stat', () => {
  const s = creditNoteStats([
    cn({ status: 'draft', refund_amount_cents: 700, refund_status: 'succeeded', total_cents: 700 }),
    cn({ status: 'voided', refund_amount_cents: 900, refund_status: 'succeeded', total_cents: 900 }),
  ])
  assert.deepEqual(s.totalRefunded, usd(0))
  assert.deepEqual(s.totalAmount, usd(0))
  assert.equal(s.draft, 1)
  assert.equal(s.voided, 1)
})

test('mixed-allocation CN contributes each channel to its own stat', () => {
  const s = creditNoteStats([
    cn({ refund_amount_cents: 1000, refund_status: 'failed', credit_amount_cents: 2000, total_cents: 3000 }),
  ])
  assert.deepEqual(s.totalCredited, usd(2000))  // the credit leg settled at Issue
  assert.deepEqual(s.totalRefunded, usd(0))     // the card leg did not
  assert.deepEqual(s.refundFailedCents, usd(1000))
  assert.deepEqual(s.totalAmount, usd(3000))
})

// A EUR credit note was added into the dollar total: €50 and $30 read "$80.00".
test('money stats are summed per currency, never across currencies', () => {
  const s = creditNoteStats([
    cn({ currency: 'USD', refund_amount_cents: 3000, refund_status: 'succeeded', total_cents: 3000 }),
    cn({ currency: 'EUR', credit_amount_cents: 5000, total_cents: 5000 }),
    cn({ currency: 'EUR', refund_amount_cents: 700, refund_status: 'failed', total_cents: 700 }),
  ])
  assert.deepEqual(s.totalAmount, [
    { currency: 'EUR', amount_cents: 5700 },
    { currency: 'USD', amount_cents: 3000 },
  ])
  assert.deepEqual(s.totalRefunded, usd(3000))
  assert.deepEqual(s.totalCredited, [{ currency: 'EUR', amount_cents: 5000 }])
  assert.deepEqual(s.refundFailedCents, [{ currency: 'EUR', amount_cents: 700 }])
})
