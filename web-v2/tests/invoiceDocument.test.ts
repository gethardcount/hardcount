// The invoice page never invents a seller and never shows a draft as issued
// (it used to show "HARDCOUNT" / "Your Company" and "Issued Oct 16" on a draft).
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { hasDocument, issueDates, sellerBlock, sellerMissing, showsActivity } from '../src/lib/invoiceDocument.ts'

const fmt = (iso: string) => iso.slice(0, 10)

test('an issued invoice uses its own seller copy, even an empty one', () => {
  const inv = { supplier_snapshot: { name: '' } }
  assert.equal(sellerBlock(inv, { company_name: 'Live Co' }).name, '')
  assert.equal(sellerMissing(inv, { company_name: 'Live Co' }, true), true)
  assert.equal(sellerBlock({ supplier_snapshot: { name: 'Acme Ltd', city: 'Pune', country: 'IN' } }).addressLines.join('|'), 'Pune|IN')
})

test('a draft uses live settings; no name means missing, never a placeholder', () => {
  const draft = { supplier_snapshot: null }
  assert.equal(sellerBlock(draft, { company_name: '  ' }).name, '')
  assert.equal(sellerMissing(draft, { company_name: '' }, true), true)
  assert.equal(sellerMissing(draft, { company_name: 'Acme' }, true), false)
})

test('the missing-seller warning waits for settings to load', () => {
  assert.equal(sellerMissing({}, undefined, false), false)
})

test('a composed draft is not issued and gets its due date at finalize', () => {
  const d = issueDates({ status: 'draft', billing_reason: 'manual', issued_at: '2026-10-16T00:00:00Z', due_at: '2026-11-15T00:00:00Z', net_payment_term_days: 30 }, fmt)
  assert.equal(d.issued, 'Draft — not issued')
  assert.equal(d.due, '—')
  assert.equal(d.dueHint, 'Net 30 from finalizing')
  assert.equal(issueDates({ status: 'draft', billing_reason: 'manual', net_payment_term_days: 0 }, fmt).dueHint, 'Due on receipt once finalized')
})

test('a draft with no billing reason counts as composed, as the server does', () => {
  const d = issueDates({ status: 'draft', billing_reason: '', due_at: '2026-11-15T00:00:00Z', net_payment_term_days: 30 }, fmt)
  assert.equal(d.due, '—')
  assert.equal(d.dueHint, 'Net 30 from finalizing')
  assert.equal(issueDates({ status: 'draft', due_at: '2026-11-15T00:00:00Z', net_payment_term_days: 30 }, fmt).due, '—')
})

test('a cycle draft keeps the due date it was built with', () => {
  const d = issueDates({ status: 'draft', billing_reason: 'subscription_cycle', due_at: '2026-11-12T00:00:00Z' }, fmt)
  assert.deepEqual(d, { issued: 'Draft — not issued', due: '2026-11-12' })
})

test('an open invoice shows its issue and due dates', () => {
  const d = issueDates({ status: 'finalized', issued_at: '2026-10-16T00:00:00Z', due_at: '2026-11-15T00:00:00Z' }, fmt)
  assert.deepEqual(d, { issued: '2026-10-16', due: '2026-11-15' })
})

test('a top-up has no due date, is issued by its payment, and one withdrawn was never issued', () => {
  assert.deepEqual(issueDates({ status: 'draft', billing_reason: 'topup', net_payment_term_days: 30 }, fmt),
    { issued: 'Issued when paid', due: '—', dueHint: 'No due date' })
  assert.deepEqual(issueDates({ status: 'voided', billing_reason: 'topup', created_at: '2026-10-16T00:00:00Z' }, fmt),
    { issued: 'Never issued', due: '—' })
  // Paid, it is an issued invoice.
  assert.deepEqual(issueDates({ status: 'paid', billing_reason: 'topup', issued_at: '2026-10-16T00:00:00Z' }, fmt),
    { issued: '2026-10-16', due: '—' })
})

// The server refuses the PDF of a top-up not issued, and its timeline answers
// for an unpaid one: the page offers the PDF and shows Activity by the same rules.
test('a top-up has a PDF only once paid, and Activity while unpaid', () => {
  const topup = (status: string) => ({ status, billing_reason: 'topup' })
  assert.deepEqual(['draft', 'voided', 'paid'].map(st => hasDocument(topup(st))), [false, false, true])
  assert.equal(hasDocument({ status: 'draft', billing_reason: 'manual' }), true)
  assert.equal(hasDocument({ status: 'voided', billing_reason: 'manual' }), true)
  assert.equal(showsActivity(topup('draft')), true)
  assert.equal(showsActivity({ status: 'draft', billing_reason: 'manual' }), false)
  assert.equal(showsActivity({ status: 'finalized', billing_reason: 'manual' }), true)
})
