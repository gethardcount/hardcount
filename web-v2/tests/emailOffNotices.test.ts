// Team invites and Forgot password each have two versions: email set up or
// not. The off version must never promise an email; the on version must not
// show a warning.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { inviteFormState, resetSentNotice } from '../src/lib/emailOffNotices.ts'

test('invites off: form disabled, amber notice, no promise of an email', () => {
  const s = inviteFormState(false)
  assert.equal(s.off, true)
  assert.ok(s.notice)
  assert.equal(s.notice.tone, 'amber')
  assert.equal(s.notice.title, 'Invites are off')
  assert.equal(s.notice.operatorDetails, true)
  assert.doesNotMatch(s.notice.body, /SMTP|DASHBOARD_BASE_URL/)
})

test('invites on: form enabled, no notice', () => {
  assert.deepEqual(inviteFormState(true), { off: false, notice: null })
})

test('invites while whoami loads: form stays on, no warning flash', () => {
  assert.deepEqual(inviteFormState(undefined), { off: false, notice: null })
})

test('reset with email off: says none was sent, Details for the operator', () => {
  const n = resetSentNotice('not_configured', 'a@b.co')
  assert.equal(n.tone, 'amber')
  assert.equal(n.title, 'Password reset email is off')
  assert.match(n.body, /none was sent/)
  assert.doesNotMatch(n.body, /a@b\.co|on its way|SMTP/)
  assert.equal(n.operatorDetails, true)
})

test('reset with email on: conditional, names the address, the 1-hour life and the 3-an-hour cap', () => {
  const n = resetSentNotice('ok', 'a@b.co')
  assert.equal(n.tone, 'neutral')
  assert.equal(n.title, 'Check your email')
  assert.match(n.body, /^If an account exists for a@b\.co, a reset link is on its way\./)
  assert.match(n.body, /1 hour/)
  assert.match(n.body, /at most 3 an hour/)
  assert.equal(n.operatorDetails, false)
})

test('reset with an older server that sends no email_delivery reads as sent', () => {
  assert.equal(resetSentNotice(undefined, 'x@y.z').title, 'Check your email')
})
