// Pins where the dashboard's time zone comes from: the account settings
// (GET /v1/settings). Every day or time an operator picks is sent as an
// instant in that zone. Before the settings load there is no zone, and a
// page that asks for one then would send a guess: a credit that expires
// hours early or late, a test clock set to the wrong hour. Signed-in pages
// wait for the settings (AccountSettingsGate), so the hook throws instead of
// answering 'UTC'.
//
// The hook is rendered once on the server, from a cache that holds the
// settings or not. Server rendering sends no request.
import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { getGetTenantSettingsQueryKey } from '../src/lib/gen/queries.gen.ts'

async function zoneFrom(settings?: { timezone: string }): Promise<string> {
  const { useTenantTimezone } = await import('../src/hooks/useTenantTimezone.ts')
  const client = new QueryClient()
  if (settings) client.setQueryData(getGetTenantSettingsQueryKey(), { default_currency: 'USD', ...settings })
  // The probe prints the zone, and the test reads it back.
  function Probe() {
    return useTenantTimezone()
  }
  return renderToStaticMarkup(createElement(QueryClientProvider, { client }, createElement(Probe)))
}

test('the zone is the one the account settings name', async () => {
  assert.equal(await zoneFrom({ timezone: 'Asia/Tokyo' }), 'Asia/Tokyo')
  assert.equal(await zoneFrom({ timezone: 'America/Los_Angeles' }), 'America/Los_Angeles')
})

test('before the settings load there is no zone: the hook throws, it never answers UTC', async () => {
  await assert.rejects(zoneFrom(), /account settings/)
})
