// Which invoices can take a credit note (E8 / D-13). The Credit Notes page's
// invoice picker and the invoice page's "Issue credit note" action use this
// one rule, and it is the server's: a finalized or paid invoice whose payment
// is not in flight. The invoice page offered the action on written-off
// invoices too, and the server refused every one.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { canIssueCreditNote } from '../src/lib/creditNoteCeilings.ts'

test('finalized and paid invoices can take a credit note', () => {
  assert.equal(canIssueCreditNote({ status: 'finalized', payment_status: 'pending' }), true)
  assert.equal(canIssueCreditNote({ status: 'finalized', payment_status: 'failed' }), true)
  assert.equal(canIssueCreditNote({ status: 'paid', payment_status: 'succeeded' }), true)
})

test('draft, voided and written-off invoices cannot', () => {
  for (const status of ['draft', 'voided', 'uncollectible']) {
    assert.equal(canIssueCreditNote({ status, payment_status: 'pending' }), false, status)
  }
})

test('an invoice whose payment is in flight cannot, until the payment settles', () => {
  assert.equal(canIssueCreditNote({ status: 'finalized', payment_status: 'processing' }), false)
  assert.equal(canIssueCreditNote({ status: 'finalized', payment_status: 'unknown' }), false)
})
