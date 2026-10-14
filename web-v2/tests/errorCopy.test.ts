// What an operator reads when a request fails. A toast keeps the caller's
// title ("Failed to attach meter") and puts one sentence under it; before,
// every API error threw the title away and showed a lower-case server
// string plus "Request ID: …", even for a simple validation error.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  classifyError,
  errorRequestId,
  errorSentence,
  errorToast,
  rawErrorDetail,
  SESSION_ENDED_MESSAGE,
} from '../src/lib/errorCopy.ts'

// Same shape as lib/api's ApiError (the real one pulls in the whole client).
class FakeApiError extends Error {
  status: number
  code?: string
  requestId?: string
  constructor(message: string, status: number, opts: { code?: string; requestId?: string } = {}) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = opts.code
    this.requestId = opts.requestId
  }
}

test('a refused request keeps the caller title and shows the server sentence, without an ID', () => {
  const err = new FakeApiError('a payment for this invoice is already in progress', 409, { code: 'invalid_state', requestId: 'req_1' })
  assert.deepEqual(errorToast(err, 'Payment failed'), {
    show: true,
    title: 'Payment failed',
    description: 'A payment for this invoice is already in progress.',
    copyId: undefined,
  })
})

test('email not set up: the action title over the server sentence, no Copy ID', () => {
  const err = new FakeApiError("Email isn't set up on this server, so nothing was sent.", 409, { code: 'email_not_configured', requestId: 'req_2' })
  assert.deepEqual(errorToast(err, 'Failed to send setup link'), {
    show: true,
    title: 'Failed to send setup link',
    description: "Email isn't set up on this server, so nothing was sent.",
    copyId: undefined,
  })
})

test('a server fault offers Copy ID and replaces the bare internal-error text', () => {
  const err = new FakeApiError('an internal error occurred', 500, { code: 'internal_error', requestId: 'req_9' })
  const t = errorToast(err, 'Failed to attach meter')
  assert.equal(t.title, 'Failed to attach meter')
  assert.equal(t.copyId, 'req_9')
  assert.match(t.description!, /^Something went wrong on the server/)
  // The raw text is kept for Details.
  assert.equal(rawErrorDetail(err), 'an internal error occurred')
})

test('a coded 5xx keeps its own sentence (it explains something)', () => {
  const err = new FakeApiError('Stripe is unavailable right now', 503, { code: 'stripe_unavailable', requestId: 'req_2' })
  assert.equal(errorSentence(err), 'Stripe is unavailable right now.')
  assert.equal(errorRequestId(err), 'req_2')
})

test('a 401 shows no toast: the session handler signs the operator in again', () => {
  const err = new FakeApiError('missing credentials — sign in at /login or send Authorization: Bearer vlx_secret_...', 401)
  assert.equal(classifyError(err), 'session')
  assert.equal(errorToast(err, 'Failed to save').show, false)
  // Wherever it is rendered (a load error), the API-caller text is not.
  assert.equal(errorSentence(err), SESSION_ENDED_MESSAGE)
})

test('no response: network failure and timeout read as connection problems', () => {
  assert.equal(classifyError(new TypeError('Failed to fetch')), 'offline')
  assert.equal(classifyError(new TypeError('Load failed')), 'offline')
  assert.match(errorSentence(new TypeError('NetworkError when attempting to fetch resource.')), /Couldn't reach the server/)
  const timeout = new FakeApiError('Request timed out after 40s — …', 0, { code: 'timeout' })
  assert.match(errorSentence(timeout), /didn't respond in time/)
  assert.equal(errorRequestId(timeout), undefined)
})

test('a generic 404 says why it can be missing; a coded one keeps its words', () => {
  assert.match(errorSentence(new FakeApiError('stripe_credentials could not be found', 404, { code: 'not_found' })), /other mode \(test or live\)/)
  assert.equal(errorSentence(new FakeApiError('no open invoice for this customer', 404, { code: 'invoice_missing' })), 'No open invoice for this customer.')
})

test('rate limit reads as a wait, not as server text', () => {
  assert.equal(errorSentence(new FakeApiError('too many requests — please retry after the rate limit resets', 429)), 'Too many requests. Wait a minute, then try again.')
})

test('without a caller title the sentence is the title', () => {
  const t = errorToast(new FakeApiError('name is required', 422))
  assert.equal(t.title, 'Name is required.')
  assert.equal(t.description, undefined)
})

test('a non-API error keeps the message the page threw', () => {
  assert.equal(errorSentence(new Error('Pick a customer first')), 'Pick a customer first.')
  assert.equal(errorSentence('weird'), 'Something unexpected went wrong. Try again.')
})
