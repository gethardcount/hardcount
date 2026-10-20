// What the sign-in pages say when a request fails.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { authFormError, signInRateLimitMessage } from '../src/lib/authFormError.ts'

class FakeApiError extends Error {
  status: number
  field?: string
  code?: string
  method?: string
  constructor(message: string, status: number, opts: { field?: string; code?: string; method?: string } = {}) {
    super(message)
    this.status = status
    this.field = opts.field
    this.code = opts.code
    this.method = opts.method
  }
}

test('server down: plain words, not "Failed to fetch"', () => {
  const msg = authFormError(new FakeApiError('Failed to fetch', 0, { code: 'network', method: 'POST' }))
  assert.equal(msg, "Couldn't reach the server. Check your connection and try again.")
})

test('no answer in time', () => {
  const msg = authFormError(new FakeApiError('No response from the server within 40 seconds.', 0, { code: 'timeout', method: 'POST' }))
  assert.equal(msg, "The server didn't respond in time. Check your connection and try again.")
})

test('a password rule reads as a sentence about the password', () => {
  assert.equal(
    authFormError(new FakeApiError('is too long. Use at most 72 characters; accented and non-Latin letters count as two or more', 422, { field: 'password' })),
    'Password is too long. Use at most 72 characters; accented and non-Latin letters count as two or more.',
  )
  assert.equal(
    authFormError(new FakeApiError('must be at least 12 characters', 422, { field: 'password' })),
    'Password must be at least 12 characters.',
  )
})

test('another refusal keeps the server sentence', () => {
  assert.equal(
    authFormError(new FakeApiError('this invitation has expired', 409)),
    'This invitation has expired.',
  )
})

test('too many requests', () => {
  assert.equal(authFormError(new FakeApiError('too many requests', 429)), 'Too many requests. Wait a moment, then try again.')
})

test('sign-in throttled: the server names the wait', () => {
  assert.equal(
    signInRateLimitMessage(new FakeApiError('Too many sign-in attempts. Try again in 3 minutes.', 429, { code: 'login_throttled' })),
    'Too many sign-in attempts. Try again in 3 minutes.',
  )
})

test('the per-address rate limit on sign-in keeps the generic line', () => {
  assert.equal(
    signInRateLimitMessage(new FakeApiError('too many requests — please retry after the rate limit resets', 429, { code: 'rate_limited' })),
    'Too many attempts. Please wait a moment and try again.',
  )
})
