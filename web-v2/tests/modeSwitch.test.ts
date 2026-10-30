// A test/live switch on a record's page goes to that record's list; other
// pages stay put.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { listPathFor } from '../src/lib/modeSwitch.ts'

test('a record page switches to its list', () => {
  assert.equal(listPathFor('/customers/hc_cus_1'), '/customers')
  assert.equal(listPathFor('/subscriptions/hc_sub_1'), '/subscriptions')
  assert.equal(listPathFor('/invoices/hc_inv_1'), '/invoices')
  assert.equal(listPathFor('/plans/hc_pln_1'), '/pricing')
  assert.equal(listPathFor('/meters/hc_mtr_1'), '/pricing?tab=meters')
  assert.equal(listPathFor('/test-clocks/hc_clk_1'), '/test-clocks')
  assert.equal(listPathFor('/webhooks/endpoints/we_1'), '/webhooks')
})

test('other pages stay where they are', () => {
  for (const p of ['/', '/customers', '/invoices', '/settings', '/webhooks/events', '/pricing', '/credits']) {
    assert.equal(listPathFor(p), null, p)
  }
})
