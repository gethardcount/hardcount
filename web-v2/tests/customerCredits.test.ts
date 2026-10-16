// Pins how the customer page explains credits. The defect: a fully used
// $500 grant left no trace (balance $0.00, no grant row) and Outstanding
// $4,160 sat beside a $4,660 invoice with no word about the credit.
import { test, beforeEach } from 'vitest'
import assert from 'node:assert/strict'
import { setActiveCurrency } from '../src/lib/money.ts'
import { creditTotalsLine, grantState, invoiceCreditNote, ledgerEntryLabel, splitGrants } from '../src/lib/customerCredits.ts'

beforeEach(() => setActiveCurrency('USD'))
const NOW = Date.parse('2026-10-16T12:00:00Z')

test('a fully used grant still shows in the totals', () => {
  assert.equal(
    creditTotalsLine({ balance_cents: 0, total_granted: 50000, total_used: 50000, total_expired: 0 }, 'USD'),
    'Granted $500.00 · Used $500.00',
  )
  assert.equal(creditTotalsLine({ balance_cents: 0, total_granted: 0, total_used: 0, total_expired: 0 }, 'USD'), null)
})

test('expired credit is named, in the customer currency', () => {
  assert.equal(
    creditTotalsLine({ balance_cents: 1000, total_granted: 5000, total_used: 2000, total_expired: 2000 }, 'EUR'),
    'Granted €50.00 · Used €20.00 · Expired €20.00',
  )
})

test('grant state: used up, expired, active', () => {
  assert.equal(grantState({ amount_cents: 50000, consumed_cents: 50000, remaining_cents: 0 }, NOW), 'used_up')
  assert.equal(grantState({ amount_cents: 50000, consumed_cents: 1000, remaining_cents: 0, expires_at: '2026-10-01T00:00:00Z' }, NOW), 'expired')
  assert.equal(grantState({ amount_cents: 50000, consumed_cents: 0, remaining_cents: 50000, expires_at: '2026-12-01T00:00:00Z' }, NOW), 'active')
  assert.equal(grantState({ amount_cents: 50000, consumed_cents: 0, remaining_cents: 50000, expires_at: '2026-10-01T00:00:00Z' }, NOW), 'expired')
})

test('used-up grants are kept, behind the active ones', () => {
  const g = [
    { id: 'a', amount_cents: 100, consumed_cents: 100, remaining_cents: 0 },
    { id: 'b', amount_cents: 100, consumed_cents: 0, remaining_cents: 100 },
  ]
  const s = splitGrants(g, NOW)
  assert.deepEqual(s.active.map(x => x.id), ['b'])
  assert.deepEqual(s.finished.map(x => x.id), ['a'])
})

test('an invoice with credit applied says why amount due is lower', () => {
  assert.equal(
    invoiceCreditNote({ credits_applied_cents: 50000, amount_due_cents: 416000, currency: 'USD' }),
    '$500.00 credit applied · $4,160.00 due',
  )
  assert.equal(invoiceCreditNote({ credits_applied_cents: 0, amount_due_cents: 416000, currency: 'USD' }), null)
})

test('the ledger calls credit applied to an invoice "Applied", not "Usage"', () => {
  assert.equal(ledgerEntryLabel('usage'), 'Applied')
  assert.equal(ledgerEntryLabel('grant'), 'Grant')
})
