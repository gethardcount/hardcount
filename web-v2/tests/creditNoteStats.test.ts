// Guards the Credit Notes page's header, tabs and stat cards (E12).
//
// They were computed from the rows the page had loaded: at most 100 notes, and
// only the open tab's status. Past 100 notes every figure was short, and under
// a status tab the other tabs' counts read 0 and disappeared. The figures now
// come from GET /v1/credit-notes/stats, which counts every note and sums money
// per currency. Which refund legs count as "refunded" (succeeded only) is
// decided and tested on the server (internal/creditnote stats test); this file
// guards how the page words what the server sent.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { creditNoteStatFigures, type CreditNoteStats } from '../src/lib/creditNoteStats.ts'

const usd = (over: Partial<CreditNoteStats['by_currency'][number]> = {}) => ({
  currency: 'USD',
  total_cents: 0,
  refunded_cents: 0,
  refund_pending_cents: 0,
  refund_failed_cents: 0,
  credited_cents: 0,
  out_of_band_cents: 0,
  ...over,
})

test('counts come from the server, so every tab keeps its count', () => {
  const f = creditNoteStatFigures({ draft_count: 3, issued_count: 140, voided_count: 2, by_currency: [] }, 'USD')
  assert.deepEqual(f.counts, { all: 145, draft: 3, issued: 140, voided: 2 })
})

test('amounts in two currencies are shown side by side, never added together', () => {
  const f = creditNoteStatFigures({
    draft_count: 0, issued_count: 2, voided_count: 0,
    by_currency: [
      usd({ total_cents: 1000, refunded_cents: 1000 }),
      usd({ currency: 'EUR', total_cents: 700, credited_cents: 700 }),
    ],
  }, 'USD')
  assert.equal(f.total, '$10.00 + €7.00')
  assert.equal(f.refunded, '$10.00')
  assert.equal(f.credited, '€7.00')
})

test('with no issued notes the money figures read zero in the default currency', () => {
  const f = creditNoteStatFigures({ draft_count: 1, issued_count: 0, voided_count: 0, by_currency: [] }, 'EUR')
  assert.equal(f.total, '€0.00')
  assert.equal(f.refunded, '€0.00')
  assert.equal(f.credited, '€0.00')
  assert.equal(f.refundPending, null)
  assert.equal(f.refundFailed, null)
})

test('pending and failed refund legs are named apart from money refunded', () => {
  const f = creditNoteStatFigures({
    draft_count: 0, issued_count: 3, voided_count: 0,
    by_currency: [usd({ total_cents: 3500, refunded_cents: 1000, refund_pending_cents: 500, refund_failed_cents: 2000 })],
  }, 'USD')
  assert.equal(f.refunded, '$10.00')
  assert.equal(f.refundPending, '$5.00')
  assert.equal(f.refundFailed, '$20.00')
})
