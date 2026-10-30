// A 429 on a dashboard load shows the error state at once and loads again
// when Retry-After says the next request will pass. Before, React Query
// retried it like any failure, and the page sat on its skeleton meanwhile.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import {
  parseRetryAfter,
  rateLimitWaitMs,
  retryQuery,
  scheduleRateLimitRefetch,
  RATE_LIMIT_DEFAULT_WAIT_MS,
  RATE_LIMIT_MAX_WAIT_MS,
} from '../src/lib/rateLimitRetry.ts'
import { errorSentence, loadErrorSentence, RATE_LIMIT_RETRYING_MESSAGE } from '../src/lib/errorCopy.ts'

// Same shape as lib/api's ApiError (the real one pulls in the whole client).
class FakeApiError extends Error {
  status: number
  retryAfterMs?: number
  constructor(message: string, status: number, retryAfterMs?: number) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.retryAfterMs = retryAfterMs
  }
}

test('parseRetryAfter reads seconds and HTTP dates', () => {
  assert.equal(parseRetryAfter('2'), 2000)
  assert.equal(parseRetryAfter(' 0 '), 0)
  const now = Date.parse('2026-10-31T09:00:00Z')
  assert.equal(parseRetryAfter('Sat, 31 Oct 2026 09:00:07 GMT', now), 7000)
  assert.equal(parseRetryAfter('Sat, 31 Oct 2026 08:59:00 GMT', now), 0)
  assert.equal(parseRetryAfter(null), undefined)
  assert.equal(parseRetryAfter(''), undefined)
  assert.equal(parseRetryAfter('soon'), undefined)
  assert.equal(parseRetryAfter('1.5'), undefined)
})

test('a 429 waits for Retry-After, within 1s to 60s; anything else does not wait', () => {
  assert.equal(rateLimitWaitMs(new FakeApiError('too many requests', 429, 3000)), 3000)
  assert.equal(rateLimitWaitMs(new FakeApiError('too many requests', 429, 0)), 1000)
  assert.equal(rateLimitWaitMs(new FakeApiError('too many requests', 429, 600_000)), RATE_LIMIT_MAX_WAIT_MS)
  assert.equal(rateLimitWaitMs(new FakeApiError('too many requests', 429)), RATE_LIMIT_DEFAULT_WAIT_MS)
  assert.equal(rateLimitWaitMs(new FakeApiError('boom', 500)), undefined)
  assert.equal(rateLimitWaitMs(new Error('not an API error')), undefined)
})

test('retryQuery: a 429 is not retried in place; other failures get one retry', () => {
  const rate = new FakeApiError('too many requests', 429, 1000)
  assert.equal(retryQuery(0, rate), false)
  const server = new FakeApiError('boom', 500)
  assert.equal(retryQuery(0, server), true)
  assert.equal(retryQuery(1, server), false)
})

test('scheduleRateLimitRefetch loads again after the wait, only while the query is shown and failed', () => {
  const timers: { fn: () => void; ms: number }[] = []
  const setTimer = (fn: () => void, ms: number) => {
    timers.push({ fn, ms })
    return 0 as unknown as ReturnType<typeof setTimeout>
  }
  let observers = 1
  let status = 'error'
  const query = { getObserversCount: () => observers, state: { get status() { return status } } }
  let refetches = 0
  const refetch = () => { refetches++ }

  scheduleRateLimitRefetch(new FakeApiError('too many requests', 429, 2000), query, refetch, setTimer)
  assert.equal(timers.length, 1)
  assert.equal(timers[0].ms, 2000)
  timers[0].fn()
  assert.equal(refetches, 1, 'shown and still failed: loads again')

  scheduleRateLimitRefetch(new FakeApiError('too many requests', 429, 2000), query, refetch, setTimer)
  observers = 0
  timers[1].fn()
  assert.equal(refetches, 1, 'no page shows it any more: sends nothing')

  scheduleRateLimitRefetch(new FakeApiError('too many requests', 429, 2000), query, refetch, setTimer)
  observers = 1
  status = 'success'
  timers[2].fn()
  assert.equal(refetches, 1, 'already loaded by something else: sends nothing')

  assert.equal(scheduleRateLimitRefetch(new FakeApiError('boom', 500), query, refetch, setTimer), undefined)
  assert.equal(timers.length, 3, 'a non-429 schedules nothing')
})

test('a load that got 429 says it will try again; an action keeps "wait, then try again"', () => {
  const rate = new FakeApiError('too many requests', 429, 1000)
  assert.equal(loadErrorSentence(rate), RATE_LIMIT_RETRYING_MESSAGE)
  assert.equal(RATE_LIMIT_RETRYING_MESSAGE, 'Too many requests. Trying again in a moment.')
  assert.equal(errorSentence(rate), 'Too many requests. Wait a moment, then try again.')
  const server = new FakeApiError('boom', 500)
  assert.equal(loadErrorSentence(server), errorSentence(server))
})
