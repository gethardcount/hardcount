// Pins the sign-in check that catches a dropped session cookie. The bug it
// prevents: on a plain-HTTP install opened by IP address, the server accepts
// the password, the browser discards the Secure cookie, and the operator is
// returned to the sign-in page with no explanation.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { confirmSessionKept, sessionNotKeptMessage, signInRefusedMessage, plainHTTPSignInMessage, SessionNotKeptError } from '../src/lib/sessionKept.ts'

const unauthorized = { status: 401 }
const is401 = (e: unknown) => (e as { status?: number })?.status === 401

test('a 401 right after sign-in means the browser dropped the cookie', async () => {
  await assert.rejects(
    confirmSessionKept(async () => { throw unauthorized }, is401),
    SessionNotKeptError,
  )
})

test('a kept session passes', async () => {
  await confirmSessionKept(async () => ({ user_id: 'u_1' }), is401)
})

test('a transient failure does not block a sign-in that worked', async () => {
  await confirmSessionKept(async () => { throw { status: 503 } }, is401)
  await confirmSessionKept(async () => { throw new TypeError('network') }, is401)
})

test('over plain HTTP the message names HTTPS and localhost', () => {
  const msg = sessionNotKeptMessage(false)
  assert.match(msg, /HTTPS/)
  assert.match(msg, /localhost/)
})

test('on a secure page the message points at cookies, not HTTPS', () => {
  const msg = sessionNotKeptMessage(true)
  assert.match(msg, /cookies/)
  assert.doesNotMatch(msg, /HTTPS/)
})

test('a refused sign-in on a plain-HTTP page explains plain HTTP', () => {
  assert.equal(signInRefusedMessage(false, 'cross-site request blocked'), plainHTTPSignInMessage)
})

test('a refused sign-in on a secure page keeps the server message', () => {
  assert.equal(signInRefusedMessage(true, 'cross-site request blocked'), 'cross-site request blocked')
})
