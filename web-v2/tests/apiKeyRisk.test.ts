// Revoking a live key, secret or usage-only, needs the typed word; a test
// key gets the plain confirm.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { revokeNeedsTypedWord } from '../src/lib/apiKeyRisk.ts'

test('revoking a live key needs the typed word', () => {
  assert.equal(revokeNeedsTypedWord({ key_prefix: 'hc_secret_live_ab12' }), true)
  // A live usage-only key carries the usage that gets billed.
  assert.equal(revokeNeedsTypedWord({ key_prefix: 'hc_ingest_live_ab12' }), true)
  // A legacy prefix with no mode is live.
  assert.equal(revokeNeedsTypedWord({ key_prefix: 'hc_secret_ab12' }), true)
  assert.equal(revokeNeedsTypedWord({ key_prefix: 'hc_secret_test_ab12' }), false)
  assert.equal(revokeNeedsTypedWord({ key_prefix: 'hc_ingest_test_ab12' }), false)
})
