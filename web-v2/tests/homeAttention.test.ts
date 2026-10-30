// Each Home line counts the rows of the list it opens (C-04).
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { homeAttentionLines } from '../src/lib/homeAttention.ts'

test('no counts, or zero counts: no lines', () => {
  assert.deepEqual(homeAttentionLines({}), [])
  assert.deepEqual(homeAttentionLines({ failed: 0, retrying: 0, open: 0, refunds: 0 }), [])
})

test('failed payments count open invoices and open that list, with no period', () => {
  const [line] = homeAttentionLines({ failed: 3 })
  assert.equal(line.text, '3 open invoices with a failed payment')
  assert.equal(line.to, '/invoices?status=finalized&payment_status=failed')
  assert.equal(homeAttentionLines({ failed: 1 })[0].text, '1 open invoice with a failed payment')
})

test('payment retry opens Dunning filtered to active runs', () => {
  const [line] = homeAttentionLines({ retrying: 2 })
  assert.equal(line.text, '2 invoices in payment retry')
  assert.equal(line.to, '/dunning?state=active')
})

test('open invoices show the amount owed only when no test-clock invoice is among them', () => {
  const usd = [{ currency: 'USD', amount_cents: 12500 }]
  assert.equal(homeAttentionLines({ open: 4, openOutsideClocks: 4, outstanding: usd })[0].text, '4 open invoices · $125.00 outstanding')
  // Test-clock invoices are in the list but not in the amount: count only.
  assert.equal(homeAttentionLines({ open: 4, openOutsideClocks: 1, outstanding: usd })[0].text, '4 open invoices')
  // All on test clocks: the line still shows (it used to hide).
  assert.equal(homeAttentionLines({ open: 2, openOutsideClocks: 0, outstanding: [] })[0].text, '2 open invoices')
})
