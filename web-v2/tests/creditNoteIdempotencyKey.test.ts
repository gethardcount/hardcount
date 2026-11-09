// "Issue credit note" sends an Idempotency-Key, and sends the same key again
// on a retry (#1273). Without the key, a retry after a lost answer ran the
// action twice: a second credit note and a second card refund. With it, the
// server answers the retry with the first request's answer.
import { afterEach, test, vi } from 'vitest'
import assert from 'node:assert/strict'
import { api, type CreditNote } from '../src/lib/api.ts'
import { outcomeUnknown } from '../src/lib/errorCopy.ts'

type CreditNoteBody = Parameters<typeof api.createCreditNote>[0]

// The call as both dialogs make it: the request body and the dialog's key.
const issueCreditNote: (body: CreditNoteBody, idempotencyKey: string) => Promise<CreditNote> = api.createCreditNote

const body: CreditNoteBody = {
  invoice_id: 'hc_inv_test',
  reason: 'Billing error',
  refund_amount_cents: 1000,
  credit_amount_cents: 0,
  out_of_band_amount_cents: 0,
  auto_issue: true,
  lines: [{ description: 'Billing error', quantity: 1, unit_amount_cents: 1000 }],
}

afterEach(() => {
  vi.unstubAllGlobals()
})

test('a retry after a lost answer sends the same Idempotency-Key as the first try', async () => {
  const sentKeys: (string | null)[] = []
  vi.stubGlobal('fetch', async (_url: RequestInfo | URL, init?: RequestInit) => {
    sentKeys.push(new Headers(init?.headers).get('Idempotency-Key'))
    // The first answer never arrives, as when a slow refund outlasts the
    // browser's wait. The server made the note all the same.
    if (sentKeys.length === 1) throw new TypeError('Failed to fetch')
    return new Response(JSON.stringify({ id: 'hc_cn_test' }), {
      status: 201,
      headers: { 'Content-Type': 'application/json', 'Idempotent-Replayed': 'true' },
    })
  })

  const key = 'b6f0c7e2-6a4f-4f57-9a43-2f1d8c3e5a10'
  await assert.rejects(issueCreditNote(body, key))
  await issueCreditNote(body, key)

  assert.deepEqual(sentKeys, [key, key])
})

// The proxy in front of the API answers 502 with an HTML page while the API
// restarts, which may be after the note was made. The error must carry the
// request's method, so the dashboard can tell this change from a load and
// say the result is unknown instead of "failed".
test('a bare 502 from the proxy on Issue credit note reads as a result unknown', async () => {
  vi.stubGlobal('fetch', async () => new Response('<html><body>502 Bad Gateway</body></html>', {
    status: 502,
    statusText: 'Bad Gateway',
    headers: { 'Content-Type': 'text/html' },
  }))

  const err: unknown = await issueCreditNote(body, 'e1d2c3b4-a5f6-4e7d-8c9b-0a1f2e3d4c5b').then(() => null, (e: unknown) => e)

  assert.ok(err instanceof Error)
  assert.equal((err as Error & { status?: number }).status, 502)
  assert.equal((err as Error & { method?: string }).method, 'POST')
  assert.equal(outcomeUnknown(err), true)
})
