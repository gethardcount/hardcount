// "Issue credit note" sends the Idempotency-Key that its dialog made when it
// opened, and sends the same key again on a retry (#1273). Without the key, a
// retry after a lost answer ran the action twice: a second credit note and a
// second card refund. With it, the server answers the retry with the first
// request's answer.
import { afterEach, test, vi } from 'vitest'
import assert from 'node:assert/strict'
import { api, type CreditNote } from '../src/lib/api.ts'

type CreditNoteBody = Parameters<typeof api.createCreditNote>[0]

// The call as both dialogs make it: the request body and the dialog's key.
const issueCreditNote: (body: CreditNoteBody, idempotencyKey: string) => Promise<CreditNote> = api.createCreditNote

const body: CreditNoteBody = {
  invoice_id: 'vlx_inv_test',
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
    return new Response(JSON.stringify({ id: 'vlx_cn_test' }), {
      status: 201,
      headers: { 'Content-Type': 'application/json', 'Idempotent-Replayed': 'true' },
    })
  })

  const key = 'b6f0c7e2-6a4f-4f57-9a43-2f1d8c3e5a10'
  await assert.rejects(issueCreditNote(body, key))
  await issueCreditNote(body, key)

  assert.deepEqual(sentKeys, [key, key])
})
