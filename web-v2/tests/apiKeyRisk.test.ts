// Revoking a live key, secret or usage-only, needs the typed word; a test
// key gets the plain confirm.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { revokeNeedsTypedWord } from '../src/lib/apiKeyRisk.ts'

test('revoking a live key needs the typed word', () => {
  assert.equal(revokeNeedsTypedWord({ livemode: true }), true)
  assert.equal(revokeNeedsTypedWord({ livemode: false }), false)
})
