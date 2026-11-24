// An account has one retry policy per mode, and an older account may have
// none. The Payment retries page must say when failed payments are not
// retried.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { retriesOffNotice } from '../src/lib/dunningCoverage.ts'

test('no policy: retries are not set up', () => {
  const n = retriesOffNotice(null)
  assert.equal(n?.title, 'Retries are not set up')
  assert.equal(n?.body, 'Failed payments are not retried until you save a retry policy.')
})

test('a policy turned off: retries are turned off', () => {
  const n = retriesOffNotice({ enabled: false })
  assert.equal(n?.title, 'Retries are turned off')
  assert.match(n?.body ?? '', /not retried/)
})

test('a policy turned on needs no notice', () => {
  assert.equal(retriesOffNotice({ enabled: true }), null)
})
