// A money dialog's Idempotency-Key lives in the page that opens the dialog,
// one per target (Issue credit note: one per invoice). Before, the key lived
// in the dialog, so closing and reopening it after a lost answer sent a new
// key, and the server made a second credit note.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { newRetryKeys } from '../src/lib/retryKeys.ts'

test('a target keeps its key until it is settled, then gets a new one', () => {
  const keys = newRetryKeys()
  const first = keys.keyFor('hc_inv_a')
  assert.ok(first)
  assert.equal(keys.keyFor('hc_inv_a'), first, 'a second try sends the same key')
  keys.settle('hc_inv_a')
  const next = keys.keyFor('hc_inv_a')
  assert.ok(next)
  assert.notEqual(next, first, 'the try after a settled one is a new action')
})

test('each target has its own key', () => {
  const keys = newRetryKeys()
  const a = keys.keyFor('hc_inv_a')
  const b = keys.keyFor('hc_inv_b')
  assert.notEqual(a, b)
  keys.settle('hc_inv_b')
  assert.equal(keys.keyFor('hc_inv_a'), a, "settling one target keeps the other's key")
})

// The dialog says "Result unknown" after a refusal that comes before the
// server reads the key (a 429, say) only when an earlier try under that key
// may have made the note. On a first try, such a refusal did nothing.
test('a key remembers that a try under it may have taken effect, until it is settled', () => {
  const keys = newRetryKeys()
  keys.keyFor('hc_inv_a')
  assert.equal(keys.unknown('hc_inv_a'), false, 'a new key has no try whose result is unknown')
  keys.markUnknown('hc_inv_a')
  assert.equal(keys.unknown('hc_inv_a'), true)
  assert.equal(keys.unknown('hc_inv_b'), false, 'each target has its own')
  keys.settle('hc_inv_a')
  assert.equal(keys.unknown('hc_inv_a'), false, 'settled, the next try is a new action')
  // A try that ends after another one settled the key marks nothing: it
  // makes no key the next try would send.
  keys.markUnknown('hc_inv_a')
  assert.equal(keys.unknown('hc_inv_a'), false)
})
