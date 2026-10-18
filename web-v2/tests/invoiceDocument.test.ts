// The invoice page never invents a seller and never shows a draft as issued
// (it used to show "VELOX" / "Your Company" and "Issued Oct 16" on a draft).
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { issueDates, sellerBlock, sellerMissing } from '../src/lib/invoiceDocument.ts'

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
