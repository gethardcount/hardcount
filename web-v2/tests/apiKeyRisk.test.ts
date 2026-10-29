// Revoking a live secret or platform key needs the typed word; a test or
// publishable key gets the plain confirm.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { revokeNeedsTypedWord } from '../src/lib/apiKeyRisk.ts'

test('revoking a live secret or platform key needs the typed word', () => {
  assert.equal(revokeNeedsTypedWord({ key_prefix: 'hc_secret_live_ab12', key_type: 'secret' }), true)
  assert.equal(revokeNeedsTypedWord({ key_prefix: 'hc_platform_live_ab12', key_type: 'platform' }), true)
  // A legacy prefix with no mode is live.
  assert.equal(revokeNeedsTypedWord({ key_prefix: 'hc_secret_ab12', key_type: 'secret' }), true)
  assert.equal(revokeNeedsTypedWord({ key_prefix: 'hc_secret_test_ab12', key_type: 'secret' }), false)
  assert.equal(revokeNeedsTypedWord({ key_prefix: 'hc_publishable_live_ab12', key_type: 'publishable' }), false)
})
