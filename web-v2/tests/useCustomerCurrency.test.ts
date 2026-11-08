// Pins when useCustomerCurrency answers. Its last answer is the account's
// default currency, from GET /v1/settings. Without the settings, the answer
// for a customer with no billing profile, plan or invoice would be a guess,
// and the Grant credits and New invoice dialogs would open in it. A customer
// with a currency of their own does not need the settings, so a slow or
// failed settings read must not hide it.
//
// The hook is rendered once on the server, from a cache that already holds
// the customer's reads. Server rendering sends no request, so a read missing
// from the cache is one still loading.
import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { QueryClient, QueryClientProvider, type QueryKey } from '@tanstack/react-query'
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { getGetTenantSettingsQueryKey } from '../src/lib/gen/queries.gen.ts'
import type { CustomerCurrency } from '../src/lib/customerCurrency.ts'

const CUSTOMER = 'hc_cus_1'

// A currency code is the loaded settings; 'loading' leaves them out of the
// cache, and 'failed' puts a failed read there.
type SettingsRead = string | 'loading' | 'failed'

interface Customer {
  profileCurrency?: string
  // The plan currency of the customer's one active subscription.
  planCurrency?: string
  // Reads of the customer that failed.
  failed?: 'profile'
}

// What the hook answers: the currency, and whether it says a read failed.
interface Answer { currency: CustomerCurrency | null; failed: boolean }

// A read that failed, as the cache holds it once its retries ran out.
// retryOnMount: false (in answer) keeps the hook from counting it as loading
// again because it would refetch on mount.
function fail(client: QueryClient, queryKey: QueryKey) {
  client.getQueryCache().build(client, { queryKey }).setState({
    status: 'error', fetchStatus: 'idle', error: new Error('500 Internal Server Error'),
    errorUpdatedAt: 1, errorUpdateCount: 1, fetchFailureCount: 2, fetchFailureReason: null,
  })
}

async function answer(settings: SettingsRead, customer: Customer = {}): Promise<Answer> {
  const { useCustomerCurrency } = await import('../src/hooks/useCustomerCurrency.ts')
  const client = new QueryClient({ defaultOptions: { queries: { retryOnMount: false } } })
  client.setQueryData(['customer-subs-upcoming', CUSTOMER], {
    data: customer.planCurrency ? [{ status: 'active', items: [{ plan_id: 'pln_1' }], created_at: '2026-10-01T00:00:00Z' }] : [],
  })
  client.setQueryData(['plans'], { data: customer.planCurrency ? [{ id: 'pln_1', currency: customer.planCurrency }] : [] })
  if (customer.failed === 'profile') fail(client, ['customer-billing-profile', CUSTOMER])
  else client.setQueryData(['customer-billing-profile', CUSTOMER], customer.profileCurrency ? { currency: customer.profileCurrency } : null)
  client.setQueryData(['customer-overview', CUSTOMER], { recent_invoices: [] })
  if (settings === 'failed') fail(client, getGetTenantSettingsQueryKey())
  else if (settings !== 'loading') client.setQueryData(getGetTenantSettingsQueryKey(), { default_currency: settings, timezone: 'UTC' })
  // The probe prints the answer as JSON, and the test reads it back.
  function Probe() {
    const read = useCustomerCurrency(CUSTOMER)
    return JSON.stringify({ currency: read.currency ?? null, failed: !!read.failure })
  }
  const html = renderToStaticMarkup(createElement(QueryClientProvider, { client }, createElement(Probe)))
  return JSON.parse(html.replace(/&quot;/g, '"'))
}

test('no currency while the account settings are loading', async () => {
  assert.deepEqual(await answer('loading'), { currency: null, failed: false })
})

test('the account default currency once the settings load', async () => {
  assert.deepEqual(await answer('EUR'), { currency: { currency: 'EUR', source: 'workspace' }, failed: false })
})

test('a billing profile currency still wins over the account default', async () => {
  assert.deepEqual(await answer('EUR', { profileCurrency: 'gbp' }),
    { currency: { currency: 'GBP', source: 'billing_profile' }, failed: false })
})

// Review of #1626: the hook waited for the settings before anything else, so
// a failed settings read hid a EUR billing profile, and Grant credits opened
// with no currency and no error.
test('a billing profile or plan currency does not wait for the account settings', async () => {
  for (const settings of ['loading', 'failed'] as const) {
    assert.deepEqual(await answer(settings, { profileCurrency: 'eur' }),
      { currency: { currency: 'EUR', source: 'billing_profile' }, failed: false }, `settings ${settings}`)
    assert.deepEqual(await answer(settings, { planCurrency: 'eur' }),
      { currency: { currency: 'EUR', source: 'subscription' }, failed: false }, `settings ${settings}`)
  }
})

test('a failed read that the answer needs is reported, not shown as an empty answer', async () => {
  // The customer has no currency of their own, so the answer is the
  // account's, and its read failed.
  assert.deepEqual(await answer('failed'), { currency: null, failed: true })
  // The billing profile read failed: its currency would win, so nothing
  // after it can answer.
  assert.deepEqual(await answer('EUR', { failed: 'profile' }), { currency: null, failed: true })
})
