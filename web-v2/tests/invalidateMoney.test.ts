// Pins the cache families a money change refreshes. The bug renders
// perfectly: after a payment or a void, the sidebar counts, the Dunning list
// and the customer's "Last invoice" kept the old state until their 30-second
// cache ran out and the page changed (sweep E10), because their keys were not
// on this list.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { QueryClient } from '@tanstack/react-query'
import { invalidateMoneySurfaces } from '../src/lib/invalidateMoney.ts'

// Each key is the one its screen uses (grep the queryKey to find it).
const MONEY_KEYS: readonly (readonly unknown[])[] = [
  ['layout', 'analytics-overview'], // sidebar counts
  ['dunning-runs', 1, ''], // Dunning list
  ['dunning-stats'], // Dunning cards
  ['invoice-dunning-runs', 'inv_1'], // the invoice page's dunning row
  ['last-invoice', 'cus_1', 'sub_1'], // customer page, "Last invoice"
  ['subscription-invoices', 'sub_1'], // subscription page, Invoices
  ['command-palette', 'invoices', ''], // command palette
  ['invoices'],
  ['invoice', 'inv_1'],
  ['dashboard-recent-invoices'],
  ['dashboard-overview', '30d'],
  ['customer-overview', 'cus_1'],
  ['customer-balance', 'cus_1'],
  ['credits'],
  ['credit-notes'],
]

test('every money surface is invalidated', () => {
  const qc = new QueryClient()
  for (const key of MONEY_KEYS) qc.setQueryData(key, 1)
  invalidateMoneySurfaces(qc)
  for (const key of MONEY_KEYS) {
    assert.equal(qc.getQueryState(key)?.isInvalidated, true, `${JSON.stringify(key)} was left stale`)
  }
})

test('a query that does not show money is left alone', () => {
  // Negative control: a function that invalidated everything would pass the
  // test above.
  const qc = new QueryClient()
  qc.setQueryData(['plans'], 1)
  qc.setQueryData(['webhook-endpoints'], 1)
  invalidateMoneySurfaces(qc)
  assert.equal(qc.getQueryState(['plans'])?.isInvalidated, false)
  assert.equal(qc.getQueryState(['webhook-endpoints'])?.isInvalidated, false)
})
