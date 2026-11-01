// A dashboard on localhost used to say "add this URL in Stripe" with a URL
// Stripe can never call. It now shows the Stripe CLI command for that case.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { isLocalAddress, stripeListenCommand, stripeWebhookUrl } from '../src/lib/stripeWebhookUrl.ts'

test('the webhook URL is the API origin plus the endpoint path', () => {
  assert.equal(stripeWebhookUrl('http://localhost:5173', 'hc_spc_1'), 'http://localhost:5173/v1/webhooks/stripe/hc_spc_1')
  assert.equal(stripeWebhookUrl('https://billing.example.com', 'hc_spc_1'), 'https://billing.example.com/v1/webhooks/stripe/hc_spc_1')
})

test('only an address on this computer counts as one Stripe cannot reach', () => {
  for (const u of ['http://localhost:5173/x', 'http://localhost/x', 'http://app.localhost:8080/x', 'http://127.0.0.1:8080/x', 'http://[::1]:8080/x']) {
    assert.equal(isLocalAddress(u), true, u)
  }
  for (const u of ['https://billing.example.com/x', 'http://10.0.0.5:8080/x', 'https://localhost.example.com/x', 'not a url']) {
    assert.equal(isLocalAddress(u), false, u)
  }
})

test('the CLI command forwards to the webhook URL', () => {
  assert.equal(stripeListenCommand('http://localhost:5173/v1/webhooks/stripe/hc_spc_1'), 'stripe listen --forward-to http://localhost:5173/v1/webhooks/stripe/hc_spc_1')
})
