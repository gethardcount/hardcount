// Pins what a signed-in page shows before the account settings load. The
// settings hold the time zone that every picked day or time is sent in, and
// the default currency. A page drawn before they load would have to guess
// both, so AccountSettingsGate shows a wait, or the error with Retry when the
// read failed, and draws the page only once it has them.
//
// The gate is rendered once on the server, from a cache that holds the
// settings, a failed read, or nothing yet. Server rendering sends no request
// and runs no effect, so a test that expects the page sets the display zone
// the gate's effect would set.
import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { test, beforeEach } from 'vitest'
import assert from 'node:assert/strict'
import { getGetTenantSettingsQueryKey } from '../src/lib/gen/queries.gen.ts'
import { setTenantTimezone } from '../src/lib/api.ts'

type SettingsRead = 'loading' | 'failed' | { timezone: string }

async function render(settings: SettingsRead): Promise<string> {
  const { AccountSettingsGate } = await import('../src/components/AccountSettingsGate.tsx')
  const client = new QueryClient({ defaultOptions: { queries: { retryOnMount: false } } })
  const key = getGetTenantSettingsQueryKey()
  if (settings === 'failed') {
    client.getQueryCache().build(client, { queryKey: key }).setState({
      status: 'error', fetchStatus: 'idle', error: new Error('500 Internal Server Error'),
      errorUpdatedAt: 1, errorUpdateCount: 1, fetchFailureCount: 2, fetchFailureReason: null,
    })
  } else if (settings !== 'loading') {
    client.setQueryData(key, { default_currency: 'USD', ...settings })
  }
  return renderToStaticMarkup(createElement(QueryClientProvider, { client },
    createElement(AccountSettingsGate, null, createElement('p', null, 'the page'))))
}

beforeEach(() => setTenantTimezone(null))

test('while the settings load, the page waits', async () => {
  const html = await render('loading')
  assert.match(html, /role="status"[^>]*aria-label="Loading your account settings"/)
  assert.doesNotMatch(html, /the page/)
})

test('when the settings read failed, it says so with Retry, and shows no page', async () => {
  const html = await render('failed')
  assert.match(html, /role="alert"/)
  assert.match(html, /Couldn(&#x27;|')t load your account settings/)
  assert.match(html, />Retry</)
  assert.doesNotMatch(html, /the page/)
})

test('once the settings and the display zone are in, the page shows', async () => {
  setTenantTimezone('Asia/Tokyo')
  assert.match(await render({ timezone: 'Asia/Tokyo' }), /the page/)
})

test('settings without the display zone set yet still wait one render', async () => {
  // formatDate reads the display zone, and the gate's effect sets it from
  // the settings. Drawn before that, a page would show browser-zone dates.
  assert.doesNotMatch(await render({ timezone: 'Asia/Tokyo' }), /the page/)
})
