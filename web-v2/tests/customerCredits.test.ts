// Pins how the customer page explains credits. The defect: a fully used
// $500 grant left no trace (balance $0.00, no grant row) and Outstanding
// $4,160 sat beside a $4,660 invoice with no word about the credit.
import { test, beforeEach } from 'vitest'
import assert from 'node:assert/strict'
import { setActiveCurrency } from '../src/lib/money.ts'
import { creditTotalsLine, grantState, grantUsedCents, invoiceCreditNote, ledgerEntryLabel, ledgerEntryTitle, outstandingIncludesCredit, splitGrants } from '../src/lib/customerCredits.ts'

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

test('canceled and deducted credit are named, so the totals add up to granted', () => {
  // After a commit void: granted $100, used $40, $60 canceled. Without the
  // Canceled part the line left $60 unexplained.
  assert.equal(
    creditTotalsLine({ balance_cents: 0, total_granted: 10000, total_used: 4000, total_expired: 0, total_retired: 6000, total_deducted: 0 }, 'USD'),
    'Granted $100.00 · Used $40.00 · Canceled $60.00',
  )
  assert.equal(
    creditTotalsLine({ balance_cents: 2000, total_granted: 15000, total_used: 3000, total_expired: 1000, total_retired: 0, total_deducted: 9000 }, 'USD'),
    'Granted $150.00 · Used $30.00 · Expired $10.00 · Deducted $90.00',
  )
})

test('grant state: used up, expired, active', () => {
  assert.equal(grantState({ amount_cents: 50000, consumed_cents: 50000, remaining_cents: 0 }, NOW), 'used_up')
  assert.equal(grantState({ amount_cents: 50000, consumed_cents: 0, remaining_cents: 50000, expires_at: '2026-12-01T00:00:00Z' }, NOW), 'active')
  assert.equal(grantState({ amount_cents: 50000, consumed_cents: 0, remaining_cents: 50000, expires_at: '2026-10-01T00:00:00Z' }, NOW), 'expired')
})

test('an expired grant is not "used up", and its expired part is not counted as used', () => {
  // The expiry sweep sets consumed to the full amount: $30 applied, $70
  // expired reads consumed $100. The server names the expired part.
  const g = { amount_cents: 10000, consumed_cents: 10000, remaining_cents: 0, expired_cents: 7000, expires_at: '2026-10-01T00:00:00Z' }
  assert.equal(grantState(g, NOW), 'expired')
  assert.equal(grantUsedCents(g), 3000)
  const usedUp = { amount_cents: 10000, consumed_cents: 10000, remaining_cents: 0, expired_cents: 0 }
  assert.equal(grantState(usedUp, NOW), 'used_up')
  assert.equal(grantUsedCents(usedUp), 10000)
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

test('expiry and usage entries read in plain words, not the server ids', () => {
  assert.equal(ledgerEntryTitle({ entry_type: 'expiry', description: 'Expired grant vlx_ccl_123' }), 'Expired')
  assert.equal(ledgerEntryTitle({ entry_type: 'usage', description: 'Applied to invoice vlx_inv_9' }), 'Applied')
  assert.equal(ledgerEntryTitle({ entry_type: 'grant', description: 'Welcome credit' }), 'Welcome credit')
  assert.equal(ledgerEntryTitle({ entry_type: 'adjustment', description: '' }), 'Adjustment')
})

test('"after credit applied" is said only when an unpaid invoice had credit', () => {
  const paid = { status: 'paid', payment_status: 'succeeded', credits_applied_cents: 50000 }
  const openNoCredit = { status: 'finalized', payment_status: 'pending', credits_applied_cents: 0 }
  const openWithCredit = { status: 'finalized', payment_status: 'pending', credits_applied_cents: 50000 }
  const voided = { status: 'voided', payment_status: 'pending', credits_applied_cents: 50000 }
  assert.equal(outstandingIncludesCredit([paid, openNoCredit]), false)
  assert.equal(outstandingIncludesCredit([voided]), false)
  assert.equal(outstandingIncludesCredit([paid, openWithCredit]), true)
})

test('commit credit taken back with its invoice is "canceled", not used', () => {
  // A $100 commit, $40 drawn, then its funding invoice voided: the void
  // retires the $60 left and bumps consumed to $100.
  const voided = { amount_cents: 10000, consumed_cents: 10000, remaining_cents: 0, expired_cents: 0, retired_cents: 6000 }
  assert.equal(grantState(voided, NOW), 'canceled')
  assert.equal(grantUsedCents(voided), 4000)
  // A credit note relieved $25 of a live commit: still active, $40 used.
  const relieved = { amount_cents: 10000, consumed_cents: 6500, remaining_cents: 3500, expired_cents: 0, retired_cents: 2500 }
  assert.equal(grantState(relieved, NOW), 'active')
  assert.equal(grantUsedCents(relieved), 4000)
  // Relieved in part, then the rest expired: expiry ended it.
  const thenExpired = { amount_cents: 10000, consumed_cents: 10000, remaining_cents: 0, expired_cents: 3500, retired_cents: 2500, expires_at: '2026-10-01T00:00:00Z' }
  assert.equal(grantState(thenExpired, NOW), 'expired')
  assert.equal(grantUsedCents(thenExpired), 4000)
})

test('a deduction is not counted as used, and a fully deducted grant reads Deducted', () => {
  // $100 grant: $30 applied to an invoice, $70 deducted by an operator. The
  // drain bumps consumed for both.
  const mixed = { amount_cents: 10000, consumed_cents: 10000, remaining_cents: 0, deducted_cents: 7000 }
  assert.equal(grantUsedCents(mixed), 3000)
  assert.equal(grantState(mixed, NOW), 'used_up')
  const partial = { amount_cents: 10000, consumed_cents: 5000, remaining_cents: 5000, deducted_cents: 2000 }
  assert.equal(grantUsedCents(partial), 3000)
  assert.equal(grantState(partial, NOW), 'active')
  const deducted = { amount_cents: 5000, consumed_cents: 5000, remaining_cents: 0, deducted_cents: 5000 }
  assert.equal(grantUsedCents(deducted), 0)
  assert.equal(grantState(deducted, NOW), 'deducted')
})
