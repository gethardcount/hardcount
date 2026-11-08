// Pins that useCustomerCurrency waits for the account settings. Its last
// answer is the account's default currency, from GET /v1/settings. Without
// the settings, the answer for a customer with no billing profile, plan or
// invoice would be a guess, and the Grant credits and New invoice dialogs
// would open in it.
//
// The hook is rendered once on the server, from a cache that already holds
// the customer's reads. Server rendering sends no request, so a read missing
// from the cache is one still loading.
import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { getGetTenantSettingsQueryKey } from '../src/lib/gen/queries.gen.ts'
import type { CustomerCurrency } from '../src/lib/customerCurrency.ts'

const CUSTOMER = 'hc_cus_1'

// What the hook answers for a customer with no subscription and no invoice.
// accountCurrency undefined leaves the settings out of the cache (loading).
async function answer(accountCurrency: string | undefined, profileCurrency?: string): Promise<CustomerCurrency | undefined> {
  const { useCustomerCurrency } = await import('../src/hooks/useCustomerCurrency.ts')
  const client = new QueryClient()
  client.setQueryData(['customer-subs-upcoming', CUSTOMER], { data: [] })
  client.setQueryData(['plans'], { data: [] })
  client.setQueryData(['customer-billing-profile', CUSTOMER], profileCurrency ? { currency: profileCurrency } : null)
  client.setQueryData(['customer-overview', CUSTOMER], { recent_invoices: [] })
  if (accountCurrency) client.setQueryData(getGetTenantSettingsQueryKey(), { default_currency: accountCurrency, timezone: 'UTC' })
  // The probe prints the answer as JSON, and the test reads it back.
  function Probe() {
    return JSON.stringify(useCustomerCurrency(CUSTOMER) ?? null)
  }
  const html = renderToStaticMarkup(createElement(QueryClientProvider, { client }, createElement(Probe)))
  return JSON.parse(html.replace(/&quot;/g, '"')) ?? undefined
}

test('no currency while the account settings are loading', async () => {
  assert.equal(await answer(undefined), undefined)
})

test('the account default currency once the settings load', async () => {
  assert.deepEqual(await answer('EUR'), { currency: 'EUR', source: 'workspace' })
})

test('a billing profile currency still wins over the account default', async () => {
  assert.deepEqual(await answer('EUR', 'gbp'), { currency: 'GBP', source: 'billing_profile' })
})
