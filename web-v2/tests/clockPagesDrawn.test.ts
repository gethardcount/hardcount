// What pages draw for a record on a test clock. Once the clock has loaded,
// the invoice page's "Due in" badge, the subscription pages' "Trial ends"
// badge and the customer's cycle bar are worked out from the clock's time,
// and the simulated-time line shows that time with a link to the clock. The
// clock stands in 2031, so a page that measured against real time would show
// years, not days. While the clock list loads or after it failed, a page
// shows the stored status and dates, and nothing counted from now.
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
import { formatDate, formatDateTime, setTenantTimezone } from '../src/lib/api.ts'

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
// clock's time from the clock list, which can also be still loading or
// failed.
function loadedCache(clocks: (typeof CLOCK)[] | 'loading' | 'failed' = [CLOCK]): QueryClient {
  const client = new QueryClient({ defaultOptions: { queries: { retryOnMount: false } } })
  client.setQueryData(getGetTenantSettingsQueryKey(), { default_currency: 'USD', timezone: 'UTC' })
  if (clocks === 'failed') {
    client.getQueryCache().build(client, { queryKey: ['test-clocks'] }).setState({
      status: 'error', fetchStatus: 'idle', error: new Error('500 Internal Server Error'),
      errorUpdatedAt: 1, errorUpdateCount: 1, fetchFailureCount: 2, fetchFailureReason: null,
    })
  } else if (clocks !== 'loading') {
    client.setQueryData(['test-clocks'], { data: clocks, total: clocks.length })
  }
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

function invoicePage(client: QueryClient): Promise<string> {
  return import('../src/pages/InvoiceDetail.tsx').then(({ default: InvoiceDetailPage }) =>
    render(client, `/invoices/${INVOICE.id}`, '/invoices/:id', InvoiceDetailPage))
}

function text(html: string): string {
  return html.replace(/<[^>]+>/g, ' ').replace(/&#x27;/g, "'").replace(/\s+/g, ' ')
}

test('the invoice page counts the due date from its clock', async () => {
  const page = text(await invoicePage(loadedCache()))
  assert.match(page, /Due in 10d/)
  assert.doesNotMatch(page, /Past due/)
})

test("the invoice page's simulated-time line shows the clock's time and links to it", async () => {
  const html = await invoicePage(loadedCache())
  assert.match(text(html), new RegExp(`Simulated time ?: ${formatDateTime(CLOCK.frozen_time)}`))
  assert.match(html, /href="\/test-clocks\/hc_clk_1"[^>]*>View clock</)
})

test('the invoice page takes its clock from the invoice, not its customer or subscription', async () => {
  const client = loadedCache()
  client.removeQueries({ queryKey: ['customer'] })
  client.removeQueries({ queryKey: ['subscription'] })
  assert.match(text(await invoicePage(client)), /Due in 10d/)
})

test('while the clock list loads, the line names no time and says nothing is deleted', async () => {
  const page = text(await invoicePage(loadedCache('loading')))
  assert.match(page, /Simulated time ?\. /)
  assert.doesNotMatch(page, /deleted/)
  assert.match(page, /View clock/)
})

test('a clock list read before the clock was made does not call it deleted', async () => {
  // The cached list predates the clock; the page reads the list again
  // before saying anything about it.
  const older = { ...CLOCK, id: 'hc_clk_0' }
  const page = text(await invoicePage(loadedCache([older])))
  assert.doesNotMatch(page, /deleted/)
})

test('the subscription page counts a trial end from its clock', async () => {
  const { default: SubscriptionDetailPage } = await import('../src/pages/SubscriptionDetail.tsx')
  const client = loadedCache()
  client.removeQueries({ queryKey: ['customer'] })
  const page = text(render(client, `/subscriptions/${SUBSCRIPTION.id}`, '/subscriptions/:id', SubscriptionDetailPage))
  assert.match(page, /Trial ends in 5d/)
})

test('the subscriptions list counts a trial end from its clock', async () => {
  const { default: SubscriptionsPage } = await import('../src/pages/Subscriptions.tsx')
  const client = loadedCache()
  client.setQueryData(['subscriptions', 'limit=25&offset=0&sort=created_at&dir=desc'], { data: [SUBSCRIPTION], total: 1 })
  const page = text(render(client, '/subscriptions', '/subscriptions', SubscriptionsPage))
  assert.match(page, /Trial ends in 5d/)
})

test("the dunning list counts a run's next step from the clock list", async () => {
  const { default: DunningPage } = await import('../src/pages/Dunning.tsx')
  const client = loadedCache()
  // The run was read before the clock moved on to 2031-03-01, so its
  // effective_now still holds the old time; the list holds the new one.
  const run = {
    id: 'hc_dun_1', invoice_id: INVOICE.id, customer_id: CUSTOMER.id, policy_id: 'hc_pol_1',
    state: 'active', reason: 'payment_failed', attempt_count: 1, paused: false, resolution: '',
    next_action_at: '2031-03-04T12:00:00Z', created_at: '2031-02-28T12:00:00Z',
    effective_now: '2031-02-01T12:00:00Z', test_clock_id: CLOCK.id,
  }
  client.setQueryData(['dunning-runs', 1, ''], { runs: [run], total: 1, invoiceMap: {}, customerMap: {} })
  const page = text(render(client, '/dunning', '/dunning', DunningPage))
  assert.match(page, /in 3d/)
  assert.doesNotMatch(page, /in 31d/)
})

// Relative text a page works out from now. None of it may show while the
// clock is not known.
const RELATIVE = /Past due|Due in|Due today|Trial ends in|Day \d+ of/

test('while the clock list loads, the invoice page shows the stored status and dates, and nothing counted from now', async () => {
  const page = text(await invoicePage(loadedCache('loading')))
  assert.doesNotMatch(page, RELATIVE)
  assert.match(page, /Status ?Open/)
  assert.match(page, new RegExp(formatDate(INVOICE.due_at)))
})

test('while the clock list loads, an invoice that real time would call past due is not', async () => {
  // A clock in 2020: from real time this invoice is years past due, and
  // from its clock it is due in 10 days.
  const past = { ...CLOCK, frozen_time: '2020-03-01T12:00:00Z' }
  const invoice = { ...INVOICE, issued_at: '2020-03-01T00:00:00Z', due_at: '2020-03-11T00:00:00Z' }
  const loading = loadedCache('loading')
  loading.setQueryData(['invoice', INVOICE.id], { invoice, line_items: [] })
  const page = text(await invoicePage(loading))
  assert.doesNotMatch(page, RELATIVE)
  assert.match(page, /Status ?Open/)

  const loaded = loadedCache([past])
  loaded.setQueryData(['invoice', INVOICE.id], { invoice, line_items: [] })
  const drawn = text(await invoicePage(loaded))
  assert.match(drawn, /Due in 10d/)
  assert.doesNotMatch(drawn, /Past due/)
})

test('when the clock list failed, the invoice page shows its load error once and nothing counted from now', async () => {
  const html = await invoicePage(loadedCache('failed'))
  const page = text(html)
  assert.equal(page.match(/Couldn't load test clocks/g)?.length, 1)
  assert.match(html, />Retry</)
  assert.doesNotMatch(page, RELATIVE)
  assert.match(page, /Status ?Open/)
})

function subscriptionsPage(client: QueryClient): Promise<string> {
  return import('../src/pages/Subscriptions.tsx').then(({ default: SubscriptionsPage }) => {
    client.setQueryData(['subscriptions', 'limit=25&offset=0&sort=created_at&dir=desc'], { data: [SUBSCRIPTION], total: 1 })
    return text(render(client, '/subscriptions', '/subscriptions', SubscriptionsPage))
  })
}

test('while the clock list loads, the subscriptions list shows the stored status and no trial countdown', async () => {
  const page = await subscriptionsPage(loadedCache('loading'))
  assert.doesNotMatch(page, RELATIVE)
  assert.match(page, /Trialing/)
})

test('when the clock list failed, the subscriptions list shows its load error once and no trial countdown', async () => {
  const page = await subscriptionsPage(loadedCache('failed'))
  assert.equal(page.match(/Couldn't load test clocks/g)?.length, 1)
  assert.doesNotMatch(page, RELATIVE)
  assert.match(page, /Trialing/)
})

// The customer page's upcoming-invoice card: its cycle bar counts the day of
// the period from the customer's clock. The period runs 2031-02-20 to
// 2031-03-20, and the clock stands at noon on 2031-03-01, day 10 of 28.
function upcomingCard(client: QueryClient): Promise<string> {
  return import('../src/components/CostDashboard.tsx').then(({ CostDashboard }) => {
    const sub = {
      ...SUBSCRIPTION, status: 'active', trial_end_at: null,
      current_billing_period_start: '2031-02-20T00:00:00Z', current_billing_period_end: '2031-03-20T00:00:00Z',
    }
    client.setQueryData(['customer-subs-upcoming', CUSTOMER.id], { data: [sub], total: 1 })
    return text(renderToStaticMarkup(createElement(QueryClientProvider, { client },
      createElement(MemoryRouter, null, createElement(CostDashboard, { customerId: CUSTOMER.id, customer: CUSTOMER as never })))))
  })
}

test("the customer's cycle bar counts the day from its clock", async () => {
  assert.match(await upcomingCard(loadedCache()), /Day 10 of 28/)
})

test("while the clock list loads, the customer's cycle bar shows its period and no day count", async () => {
  const page = await upcomingCard(loadedCache('loading'))
  assert.doesNotMatch(page, RELATIVE)
  assert.match(page, new RegExp(formatDate('2031-02-20T00:00:00Z')))
})
