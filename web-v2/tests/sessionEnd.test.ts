// A session that ends while a tab is open sends the operator to sign-in, but
// only after the server confirms it: a 401 from one endpoint must not sign
// anyone out, and many failing requests must cause one check, not many.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { createSessionEndWatcher, isSessionProbePath } from '../src/lib/sessionEnd.ts'

const unauthorized = { status: 401 }
const isUnauthorized = (e: unknown) => (e as { status?: number })?.status === 401
const flush = () => new Promise(r => setTimeout(r, 0))

function setup(opts: { signedIn?: boolean; confirm: () => Promise<unknown> }) {
  let ended = 0
  let checks = 0
  let signedIn = opts.signedIn ?? true
  const w = createSessionEndWatcher({
    isSignedIn: () => signedIn,
    confirm: () => { checks++; return opts.confirm() },
    isUnauthorized,
    onEnded: () => { ended++; signedIn = false },
  })
  return { w, get ended() { return ended }, get checks() { return checks } }
}

test('ends the session when the server confirms the cookie is gone', async () => {
  const s = setup({ confirm: () => Promise.reject(unauthorized) })
  s.w.report('/invoices/inv_1')
  await flush()
  assert.equal(s.ended, 1)
})

test('a 401 from one endpoint with a live session does nothing', async () => {
  const s = setup({ confirm: () => Promise.resolve({ user_id: 'u1' }) })
  s.w.report('/customers')
  await flush()
  assert.equal(s.checks, 1)
  assert.equal(s.ended, 0)
})

test('a failed check that is not a 401 (server down) does not sign out', async () => {
  const s = setup({ confirm: () => Promise.reject({ status: 503 }) })
  s.w.report('/customers')
  await flush()
  assert.equal(s.ended, 0)
})

test('many failing requests cause one check', async () => {
  const s = setup({ confirm: () => Promise.reject(unauthorized) })
  for (const p of ['/invoices', '/customers', '/analytics/overview']) s.w.report(p)
  await flush()
  assert.equal(s.checks, 1)
  assert.equal(s.ended, 1)
})

test('signed out already, or signing in/out: no check at all', async () => {
  const out = setup({ signedIn: false, confirm: () => Promise.reject(unauthorized) })
  out.w.report('/invoices')
  const login = setup({ confirm: () => Promise.reject(unauthorized) })
  for (const p of ['/auth/login', '/auth/logout', '/whoami', '/auth/password-reset/confirm', '/auth/accept-invite']) login.w.report(p)
  await flush()
  assert.equal(out.checks + login.checks, 0)
})

test('probe paths are only the sign-in family', () => {
  assert.equal(isSessionProbePath('/auth/mode'), false)
  assert.equal(isSessionProbePath('/invoices'), false)
  assert.equal(isSessionProbePath('/whoami'), true)
})
