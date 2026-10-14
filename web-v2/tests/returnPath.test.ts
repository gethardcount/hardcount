// After login the dashboard returns the user to the page they were opening
// (a shared invoice link should open the invoice, not the home page), but
// only to a path inside the dashboard, so a crafted link cannot send someone
// off-site after they sign in.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { returnPath } from '../src/lib/returnPath.ts'

test('returns the page the user was opening', () => {
  assert.equal(returnPath({ from: '/invoices/vlx_inv_1' }), '/invoices/vlx_inv_1')
  assert.equal(returnPath({ from: '/customers?status=active' }), '/customers?status=active')
})

test('falls back to home when there is nothing usable', () => {
  for (const s of [null, undefined, {}, { from: 42 }, { from: '' }, { from: '/login' }]) {
    assert.equal(returnPath(s), '/')
  }
})

test('never leaves the dashboard', () => {
  for (const from of ['https://evil.example', '//evil.example', '/\\evil.example', 'javascript:alert(1)', 'invoices/x']) {
    assert.equal(returnPath({ from }), '/')
  }
})
