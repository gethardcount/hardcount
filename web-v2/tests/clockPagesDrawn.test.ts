// What two pages draw for a record on a test clock that has loaded: the
// invoice page's "Due in" badge and the subscriptions list's "Trial ends"
// badge are worked out from the clock's time. The clock stands in 2031, so a
// page that measured against real time would show years, not days.
//
// The pages are rendered once on the server, from a cache that already holds
// their reads. Server rendering sends no request, so a read missing from the
// cache is one still loading.
import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { test, vi } from 'vitest'
import assert from 'node:assert/strict'
import { getGetTenantSettingsQueryKey } from '../src/lib/gen/queries.gen.ts'
import { setTenantTimezone } from '../src/lib/api.ts'

// The display zone, as AccountSettingsGate sets it once the settings load.
setTenantTimezone('UTC')

// The invoice page reads window.location for its hosted-invoice link.
vi.stubGlobal('window', { location: { origin: 'http://localhost:5173' } })

vi.mock('@/contexts/AuthContext', () => ({
  useAuth: () => ({
    user: { user_id: 'usr_1', tenant_id: 'ten_1', email: 'op@example.com', livemode: false },
    loading: false,
  }),
}))

const CLOCK = {
  id: 'hc_clk_1',
  name: 'Renewal run',
  frozen_time: '2031-03-01T12:00:00Z',
  status: 'ready',
  created_at: '2031-01-01T00:00:00Z',
}

const CUSTOMER = {
  id: 'hc_cus_1',
  external_id: 'acme',
  display_name: 'Acme',
  email: 'billing@acme.example',
  test_clock_id: CLOCK.id,
  created_at: '2031-01-01T00:00:00Z',
}

const SUBSCRIPTION = {
  id: 'hc_sub_1',
  code: 'acme-pro',
  display_name: 'Acme Pro',
  customer_id: CUSTOMER.id,
  test_clock_id: CLOCK.id,
  status: 'trialing',
  billing_time: 'calendar',
  items: [],
  trial_start_at: '2031-02-20T12:00:00Z',
  trial_end_at: '2031-03-06T12:00:00Z',
  created_at: '2031-02-20T12:00:00Z',
}

const INVOICE = {
  id: 'hc_inv_1',
  invoice_number: 'INV-0001',
  customer_id: CUSTOMER.id,
  subscription_id: SUBSCRIPTION.id,
  status: 'finalized',
  payment_status: 'pending',
  currency: 'USD',
  subtotal_cents: 2900,
  discount_cents: 0,
  tax_amount_cents: 0,
  tax_rate: 0,
  tax_name: '',
  total_amount_cents: 2900,
  amount_due_cents: 2900,
  amount_paid_cents: 0,
  credits_applied_cents: 0,
  billing_period_start: '2031-02-01T00:00:00Z',
  billing_period_end: '2031-03-01T00:00:00Z',
  billing_timezone: 'UTC',
  issued_at: '2031-03-01T00:00:00Z',
  due_at: '2031-03-11T00:00:00Z',
  created_at: '2031-03-01T00:00:00Z',
  is_simulated: true,
  test_clock_id: CLOCK.id,
}

// A cache holding the reads both pages make once loaded. A page reads a
// clock's time from the clock list.
function loadedCache(): QueryClient {
  const client = new QueryClient({ defaultOptions: { queries: { retryOnMount: false } } })
  client.setQueryData(getGetTenantSettingsQueryKey(), { default_currency: 'USD', timezone: 'UTC' })
  client.setQueryData(['test-clocks'], { data: [CLOCK], total: 1 })
  client.setQueryData(['invoice', INVOICE.id], { invoice: INVOICE, line_items: [] })
  client.setQueryData(['customer', CUSTOMER.id], CUSTOMER)
  client.setQueryData(['subscription', SUBSCRIPTION.id], SUBSCRIPTION)
  return client
}

function render(client: QueryClient, path: string, route: string, page: () => unknown): string {
  return renderToStaticMarkup(createElement(QueryClientProvider, { client },
    createElement(MemoryRouter, { initialEntries: [path] },
      createElement(Routes, null, createElement(Route, { path: route, element: createElement(page as never) })))))
}

function text(html: string): string {
  return html.replace(/<[^>]+>/g, ' ').replace(/&#x27;/g, "'").replace(/\s+/g, ' ')
}

test('the invoice page counts the due date from its clock', async () => {
  const { default: InvoiceDetailPage } = await import('../src/pages/InvoiceDetail.tsx')
  const page = text(render(loadedCache(), `/invoices/${INVOICE.id}`, '/invoices/:id', InvoiceDetailPage))
  assert.match(page, /Due in 10d/)
  assert.doesNotMatch(page, /Past due/)
})

test('the subscriptions list counts a trial end from its clock', async () => {
  const { default: SubscriptionsPage } = await import('../src/pages/Subscriptions.tsx')
  const client = loadedCache()
  client.setQueryData(['subscriptions', 'limit=25&offset=0&sort=created_at&dir=desc'], { data: [SUBSCRIPTION], total: 1 })
  const page = text(render(client, '/subscriptions', '/subscriptions', SubscriptionsPage))
  assert.match(page, /Trial ends in 5d/)
})
