// New invoice and Add line item send the Idempotency-Key their dialog made
// when it opened, and send the same key again on a retry. Without the key, a
// click after a lost answer would make a second invoice, or put a second line
// on the draft. With it, the server answers the retry with the first
// request's answer.
import { afterEach, test, vi } from 'vitest'
import assert from 'node:assert/strict'
import { api } from '../src/lib/api.ts'

// Sends `call` twice with one key. The first answer never arrives, as when
// the server is slow; the server made the invoice or line all the same.
// Returns the Idempotency-Key header of each request.
async function sendTwice(call: (key: string) => Promise<unknown>, key: string): Promise<(string | null)[]> {
  const sentKeys: (string | null)[] = []
  vi.stubGlobal('fetch', async (_url: RequestInfo | URL, init?: RequestInit) => {
    sentKeys.push(new Headers(init?.headers).get('Idempotency-Key'))
    if (sentKeys.length === 1) throw new TypeError('Failed to fetch')
    return new Response(JSON.stringify({ id: 'hc_test' }), {
      status: 201,
      headers: { 'Content-Type': 'application/json', 'Idempotent-Replayed': 'true' },
    })
  })
  await assert.rejects(call(key))
  await call(key)
  return sentKeys
}

afterEach(() => {
  vi.unstubAllGlobals()
})

const line = { description: 'Setup fee', line_type: 'add_on', quantity: 1, unit_amount_cents: 50000 }

test('New invoice: a retry after a lost answer sends the same Idempotency-Key as the first try', async () => {
  const key = '0b9e4c7a-3d1f-4e2b-8a6c-5f7d9e1b2c3a'
  const body = { customer_id: 'hc_cus_test', currency: 'USD', net_payment_term_days: 15, line_items: [line] }
  assert.deepEqual(await sendTwice(k => api.createInvoice(body, k), key), [key, key])
})

test('Add line item: a retry after a lost answer sends the same Idempotency-Key as the first try', async () => {
  const key = '7c1d2e3f-4a5b-4c6d-9e8f-0a1b2c3d4e5f'
  assert.deepEqual(await sendTwice(k => api.addInvoiceLineItem('hc_inv_test', line, k), key), [key, key])
})
