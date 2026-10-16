// Settings → Email states. The bug class: an operator only learned that this
// server can't send email from a per-invoice notice or "Invites are off".
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { emailSetupSummary, testEmailOutcome, tlsModeLabel, type EmailSettingsStatus } from '../src/lib/emailSettings.ts'

const links = [
  { env: 'HOSTED_INVOICE_BASE_URL', set: true, effect: 'a' },
  { env: 'PAYMENT_UPDATE_URL', set: true, effect: 'b' },
  { env: 'DASHBOARD_BASE_URL', set: true, effect: 'c' },
]
const ready: EmailSettingsStatus = {
  smtp_configured: true, smtp_host: 'smtp.example.com', from_address: 'billing@example.com', tls_mode: 'starttls',
  links, can_email_setup_links: true, can_send_invites: true,
}

test('no SMTP is off, whatever the links say', () => {
  const s = emailSetupSummary({ ...ready, smtp_configured: false, smtp_host: '' })
  assert.equal(s.state, 'off')
  assert.equal(s.title, "This server can't send email")
  assert.match(s.body, /SMTP_HOST/)
})

test('SMTP with a missing link is partial and names it', () => {
  const s = emailSetupSummary({ ...ready, links: [links[0], { ...links[1], set: false }, links[2]] })
  assert.equal(s.state, 'partial')
  assert.equal(s.title, 'One email link is not set up')
  assert.deepEqual(s.missingLinks.map(l => l.env), ['PAYMENT_UPDATE_URL'])
  assert.equal(emailSetupSummary({ ...ready, links: links.map(l => ({ ...l, set: false })) }).title, '3 email links are not set up')
})

test('all set is ready', () => {
  assert.equal(emailSetupSummary(ready).state, 'ready')
  assert.deepEqual(emailSetupSummary(ready).missingLinks, [])
})

test('test-send outcomes', () => {
  assert.deepEqual(testEmailOutcome({ sentTo: 'ops@acme.test' }).tone, 'success')
  assert.match(testEmailOutcome({ sentTo: 'ops@acme.test' }).text, /^Sent to ops@acme.test\./)
  assert.equal(testEmailOutcome({ code: 'email_not_configured', message: 'x' }).text, 'Not sent: this server has no SMTP server set up.')
  assert.equal(testEmailOutcome({ code: 'smtp_error', message: "The email server didn't accept the test email: 535" }).text,
    "The email server didn't accept the test email: 535")
})

test('transport labels', () => {
  assert.equal(tlsModeLabel('starttls'), 'STARTTLS (port 587)')
  assert.equal(tlsModeLabel('weird'), 'weird')
})
