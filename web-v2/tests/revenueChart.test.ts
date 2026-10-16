// Backs MANUAL_TEST FLOW U1 (Dashboard revenue chart): days with no paid invoices
// plot as zero, so two payments 25 days apart are not drawn as neighbours.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { fillRevenueBuckets } from '../src/lib/revenueChart.ts'

const now = Date.parse('2026-10-16T12:00:00Z')

test('30d: every day in the window is a bucket, missing days are zero', () => {
  const out = fillRevenueBuckets(
    [
      { date: '2026-09-20', revenue_cents: 5000, invoice_count: 1 },
      { date: '2026-10-15', revenue_cents: 7000, invoice_count: 2 },
    ],
    '30d',
    now,
  )
  // Window [Sep 16 12:00Z, Oct 16 12:00Z) touches 31 UTC days: Sep 16 … Oct 16.
  assert.equal(out.length, 31)
  assert.equal(out[0].date, '2026-09-16')
  assert.equal(out[out.length - 1].date, '2026-10-16')
  const i20 = out.findIndex(p => p.date === '2026-09-20')
  const i15 = out.findIndex(p => p.date === '2026-10-15')
  assert.equal(i15 - i20, 25, 'Sep 20 and Oct 15 are 25 buckets apart, not adjacent')
  assert.equal(out[i20].revenue_cents, 5000)
  assert.equal(out[i15].invoice_count, 2)
  assert.deepEqual(out[i20 + 1], { date: '2026-09-21', revenue_cents: 0, invoice_count: 0 })
  assert.equal(out.reduce((s, p) => s + p.revenue_cents, 0), 12000, 'zero-fill adds no revenue')
})

test('7d and 90d use day buckets over their own window', () => {
  assert.equal(fillRevenueBuckets([], '7d', now).length, 8)
  const ninety = fillRevenueBuckets([], '90d', now)
  assert.equal(ninety.length, 91)
  assert.equal(ninety[0].date, '2026-07-18')
})

test('12m uses month buckets over 365 days', () => {
  const out = fillRevenueBuckets([{ date: '2026-03', revenue_cents: 900, invoice_count: 1 }], '12m', now)
  // 365 days before Oct 16 2026 is Oct 16 2025 → Oct 2025 … Oct 2026.
  assert.equal(out.length, 13)
  assert.equal(out[0].date, '2025-10')
  assert.equal(out[12].date, '2026-10')
  assert.equal(out.find(p => p.date === '2026-03')?.revenue_cents, 900)
  assert.equal(out.find(p => p.date === '2026-04')?.revenue_cents, 0)
})

test('a server bucket outside the browser-computed window is kept, not dropped', () => {
  const out = fillRevenueBuckets([{ date: '2026-10-17', revenue_cents: 100, invoice_count: 1 }], '7d', now)
  assert.equal(out[out.length - 1].date, '2026-10-17')
  assert.equal(out[out.length - 1].revenue_cents, 100)
})

test('an unknown period falls back to 30 days, like the server', () => {
  assert.equal(fillRevenueBuckets([], 'bogus', now).length, 31)
})
