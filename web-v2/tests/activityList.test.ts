// Renders the shared <ActivityList> (ADR-128) and checks the row as an
// operator sees it: the day header, the title with its amount, the problem
// line, the meta line with links and the actor, the Recorded part last, and
// every date and time inside a <time> element (the e2e text-parity check
// strips those).
import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { MemoryRouter } from 'react-router-dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { test, beforeEach } from 'vitest'
import assert from 'node:assert/strict'
import type { ActivityLine } from '../src/lib/gen/schemas/activityLine.ts'

const finalized: ActivityLine = {
  id: 'lifecycle:finalized:hc_inv_1',
  kind: 'invoice.finalized',
  title: 'Invoice finalized',
  amount: { cents: 2473, currency: 'USD' },
  detail: [[{ text: 'Renewal for Feb 1 – Mar 1' }], [{ link: { kind: 'subscription', id: 'hc_sub_1', label: 'Starter' } }]],
  actor: { kind: 'automatic' },
  tone: 'neutral',
  occurred_at: '2027-02-01T04:30:00Z',
  recorded_at: '2026-10-18T14:44:00Z',
}

const failed: ActivityLine = {
  id: 'attempt:hc_att_1',
  kind: 'charge_attempt.failed',
  title: 'Payment failed',
  amount: { cents: 2473, currency: 'USD' },
  problem: 'Card declined: insufficient funds',
  detail: [],
  actor: { kind: 'automatic' },
  tone: 'failure',
  occurred_at: '2027-02-02T04:30:00Z',
}

async function render(lines: ActivityLine[]): Promise<string> {
  const { ActivityList, ActivityZone } = await import('../src/components/ActivityList.tsx')
  // The list reads the zone through useTenantTimezone, a settings query.
  // Server rendering runs no fetch, so it falls back to the zone set below.
  return renderToStaticMarkup(createElement(QueryClientProvider, { client: new QueryClient() },
    createElement(MemoryRouter, null,
      createElement(ActivityZone, { lines }),
      createElement(ActivityList, { lines }),
    )))
}

function text(html: string): string {
  return html.replace(/<[^>]+>/g, '').replace(/&#x27;/g, "'")
}

beforeEach(async () => {
  const { setTenantTimezone } = await import('../src/lib/api.ts')
  setTenantTimezone('Asia/Kolkata')
})

test('a row reads title · amount, time, then meta parts, actor and Recorded, in that order', async () => {
  const html = await render([finalized])
  const t = text(html)
  assert.match(t, /Times in GMT\+5:30/)
  assert.match(t, /Feb 1, 2027/)
  assert.match(t, /Invoice finalized · \$24\.7310:00 AM/)
  assert.match(t, /Renewal for Feb 1 – Mar 1 · Starter · Automatic · Recorded Oct 18, 8:14 PM/)
  // The subscription is a link to its page.
  assert.match(html, /<a [^>]*href="\/subscriptions\/hc_sub_1"[^>]*>Starter<\/a>/)
})

test('every date and time is a <time> element carrying its instant', async () => {
  const html = await render([finalized])
  // The day header carries the day on the account calendar, not an instant.
  assert.match(html, /<time dateTime="2027-02-01">Feb 1, 2027<\/time>/)
  assert.match(html, /<time dateTime="2027-02-01T04:30:00Z"[^>]*>10:00 AM<\/time>/)
  assert.match(html, /<time dateTime="2026-10-18T14:44:00Z">Oct 18, 8:14 PM<\/time>/)
  // With the <time> elements taken out, no date or clock time is left.
  const bare = text(html.replace(/<time[^>]*>.*?<\/time>/g, ''))
  assert.doesNotMatch(bare, /\d{1,2}:\d{2} [AP]M/)
  assert.doesNotMatch(bare, /Feb 1, 2027|Oct 18/)
})

test('a real-clock row has no Recorded part; a failure row has a red problem line', async () => {
  const html = await render([failed])
  assert.doesNotMatch(html, /Recorded/)
  assert.match(html, /data-tone="failure"/)
  assert.match(html, /<p class="[^"]*text-destructive[^"]*">Card declined: insufficient funds<\/p>/)
  assert.match(html, /bg-destructive/)
})

test('rows on two days get two day headers', async () => {
  const html = await render([finalized, failed])
  assert.equal((html.match(/data-activity-day/g) ?? []).length, 2)
  assert.match(text(html), /Feb 2, 2027/)
})

test('a day header carries the account-calendar day even when the UTC day is the one before', async () => {
  // 20:00 UTC on Jan 31 is Feb 1 in Kolkata.
  const html = await render([{ ...failed, occurred_at: '2027-01-31T20:00:00Z' }])
  assert.match(html, /<time dateTime="2027-02-01">Feb 1, 2027<\/time>/)
})

test('the zone header names the zone of the Recorded times too', async () => {
  const { setTenantTimezone } = await import('../src/lib/api.ts')
  setTenantTimezone('America/New_York')
  // Rows in Feb 2027 (EST), recorded in Oct 2026 (EDT).
  const t = text(await render([{ ...finalized, occurred_at: '2027-02-01T15:00:00Z' }]))
  assert.match(t, /Times in EST \/ EDT/)
})
