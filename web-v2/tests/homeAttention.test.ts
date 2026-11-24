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

test('open invoices show the amount owed, and the count alone when nothing is', () => {
  const usd = [{ currency: 'USD', amount_cents: 12500 }]
  assert.equal(homeAttentionLines({ open: 4, outstanding: usd })[0].text, '4 open invoices · $125.00 outstanding')
  // While the overview is loading, or with nothing owed: the count alone.
  assert.equal(homeAttentionLines({ open: 2 })[0].text, '2 open invoices')
  assert.equal(homeAttentionLines({ open: 2, outstanding: [] })[0].text, '2 open invoices')
})

test('customers out of credit come first, open the Credit list filtered to them, singular and plural', () => {
  const [line] = homeAttentionLines({ outOfCredit: 3, failed: 1 })
  assert.equal(line.text, '3 customers are out of credit')
  assert.equal(line.severity, 'danger')
  assert.equal(line.to, '/credits?balance=out')
  assert.equal(homeAttentionLines({ outOfCredit: 1 })[0].text, '1 customer is out of credit')
})

test('customers low on credit come next, as a warning, singular and plural', () => {
  const lines = homeAttentionLines({ outOfCredit: 2, lowOnCredit: 1, retrying: 1 })
  assert.deepEqual(lines.slice(0, 2).map(l => l.text), ['2 customers are out of credit', '1 customer is low on credit'])
  assert.equal(lines[1].severity, 'warning')
  assert.equal(lines[1].to, '/credits?balance=low')
  assert.equal(homeAttentionLines({ lowOnCredit: 4 })[0].text, '4 customers are low on credit')
})

test('no credit line at zero, while loading, or with no low-balance setting (the server answers 0)', () => {
  assert.deepEqual(homeAttentionLines({ outOfCredit: 0, lowOnCredit: 0 }), [])
  assert.deepEqual(homeAttentionLines({ outOfCredit: undefined, lowOnCredit: undefined }), [])
})
