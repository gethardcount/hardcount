// Pins where a Settings save's answer goes. The account settings are shared
// by test and live mode, but each mode has its own query cache, and the
// cache of the mode not open is kept for the next switch. A save that
// reached only the open mode's cache left the other one with the old
// default currency: after a switch, Create plan offered the old currency.
//
// The analytics figures on Home, the at-risk total on Dunning and the credit
// note totals are scoped to, or ordered by, the default currency on the
// server. When it changes, those reads are marked stale in both modes.
import { QueryClient } from '@tanstack/react-query'
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { getGetTenantSettingsQueryKey } from '../src/lib/gen/queries.gen.ts'
import type { TenantSettings } from '../src/lib/gen/schemas/tenantSettings.ts'
import { writeSavedSettings } from '../src/hooks/useTenantSettings.ts'

const settingsKey = getGetTenantSettingsQueryKey()
// One read of each family the server scopes to the default currency.
const currencyFigures = [
  ['dashboard-overview', '30d'],
  ['dashboard-chart', '30d'],
  ['dunning-stats'],
  ['credit-notes', 'stats'],
]
// A read that does not depend on the default currency.
const otherRead = ['customers']

function settings(default_currency: string, timezone = 'UTC') {
  return { default_currency, timezone } as TenantSettings
}

// A mode's cache with the settings (when given) and every read above loaded.
function modeCache(cached?: TenantSettings): QueryClient {
  const client = new QueryClient()
  if (cached) client.setQueryData(settingsKey, cached)
  for (const key of [...currencyFigures, otherRead]) client.setQueryData(key, { loaded: true })
  return client
}

const stale = (client: QueryClient, key: readonly unknown[]) => client.getQueryState(key)?.isInvalidated ?? false

test('a save reaches the settings of both modes', () => {
  const open = modeCache(settings('USD'))
  const other = modeCache(settings('USD'))
  const saved = settings('EUR', 'Asia/Kolkata')
  writeSavedSettings([open, other], saved)
  for (const client of [open, other]) assert.deepEqual(client.getQueryData(settingsKey), saved)
})

test('a new default currency marks the figures scoped to it stale in both modes', () => {
  // The other mode never loaded the settings: its figures are stale too.
  const open = modeCache(settings('USD'))
  const other = modeCache()
  writeSavedSettings([open, other], settings('EUR'))
  for (const client of [open, other]) {
    for (const key of currencyFigures) assert.equal(stale(client, key), true, `${key.join('/')} is stale`)
    assert.equal(stale(client, otherRead), false)
  }
})

test('a save that keeps the default currency leaves those figures alone', () => {
  const open = modeCache(settings('USD'))
  writeSavedSettings([open], settings('USD', 'Asia/Kolkata'))
  for (const key of currencyFigures) assert.equal(stale(open, key), false, `${key.join('/')} is fresh`)
})
