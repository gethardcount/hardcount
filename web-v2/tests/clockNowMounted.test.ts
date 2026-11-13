// @vitest-environment happy-dom
// What the clock hooks and the simulated-time line do once mounted, where
// effects run, queries fetch and pages re-render:
//   - a clock list cached before the page opened can predate the record's
//     clock, so the page reads the list again instead of calling the clock
//     deleted or measuring from real time;
//   - live mode sends no clock request;
//   - useNowFor keeps one value while its answer stays the same, and a page
//     left open draws real time as it moves.
import { createElement, useEffect } from 'react'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { afterEach, beforeEach, test, vi } from 'vitest'
import assert from 'node:assert/strict'
import { api, formatDateTime, setTenantTimezone, type TestClock } from '../src/lib/api.ts'
import { getGetTenantSettingsQueryKey } from '../src/lib/gen/queries.gen.ts'
import { useNowFor, useNowResolver, type ClockPinned } from '../src/hooks/useNow.ts'
import { TestClockBanner } from '../src/components/TestClockBanner.tsx'
import { mount, until, type Mounted } from './mount.ts'

let livemode = false
vi.mock('@/contexts/AuthContext', () => ({
  useAuth: () => ({ user: { user_id: 'usr_1', tenant_id: 'ten_1', email: 'op@example.com', livemode }, loading: false }),
}))

setTenantTimezone('UTC')

const CLOCK = { id: 'hc_clk_1', name: 'Renewal run', frozen_time: '2031-03-01T12:00:00Z', status: 'ready', created_at: '2031-01-01T00:00:00Z' } as TestClock
const OTHER = { ...CLOCK, id: 'hc_clk_0', name: 'Older run' } as TestClock

// The server's clocks, and the reads of the list it has answered.
let server: TestClock[]
let listReads: number
// release answers the list reads waiting on it; until then they are loading.
let release: () => void

beforeEach(() => {
  livemode = false
  server = [OTHER, CLOCK]
  listReads = 0
  let open!: () => void
  const gate = new Promise<void>(r => { open = r })
  release = open
  vi.stubGlobal('fetch', () => new Promise(() => {}))
  vi.spyOn(api, 'listTestClocks').mockImplementation(async () => {
    listReads++
    await gate
    return { data: server.map(c => ({ ...c })), total: server.length } as never
  })
})

let page: Mounted | undefined
afterEach(async () => {
  await page?.unmount()
  page = undefined
  vi.useRealTimers()
  vi.restoreAllMocks()
})

// A cache whose clock list was read before the page opened, holding only the
// clock that existed then. Fresh for 30 seconds, as the app's is.
function cacheReadBefore(): QueryClient {
  const client = new QueryClient({ defaultOptions: { queries: { staleTime: 30_000, retry: false } } })
  client.setQueryData(getGetTenantSettingsQueryKey(), { default_currency: 'USD', timezone: 'UTC' })
  client.setQueryData(['test-clocks'], { data: [OTHER], total: 1 })
  return client
}

function inApp(client: QueryClient, ...children: ReturnType<typeof createElement>[]) {
  return createElement(QueryClientProvider, { client }, createElement(MemoryRouter, null, ...children))
}

function NowProbe({ record }: { record: ClockPinned }) {
  return createElement('p', null, `now: ${new Date(useNowFor(record)).toISOString()}`)
}

test('a clock made after the list was read is read again, not called deleted', async () => {
  page = await mount(inApp(cacheReadBefore(), createElement(TestClockBanner, { testClockId: CLOCK.id }), createElement(NowProbe, { record: { test_clock_id: CLOCK.id } })))

  // The list is being read again: the line says nothing about the clock yet.
  assert.doesNotMatch(page.text(), /deleted/)
  assert.equal(listReads, 1)

  release()
  await until(() => assert.match(page!.text(), new RegExp(`Simulated time: ${formatDateTime(CLOCK.frozen_time)}`)))
  assert.match(page.text(), /View clock/)
  assert.match(page.text(), /now: 2031-03-01T12:00:00.000Z/)
})

test('a clock the list read again does not hold is called deleted, after one read', async () => {
  server = [OTHER]
  release()
  page = await mount(inApp(cacheReadBefore(), createElement(TestClockBanner, { testClockId: CLOCK.id })))

  await until(() => assert.match(page!.text(), /Test clock deleted\./))
  assert.doesNotMatch(page.text(), /View clock/)
  await new Promise(r => setTimeout(r, 50))
  assert.equal(listReads, 1)
})

test('live mode sends no clock request', async () => {
  livemode = true
  release()
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  page = await mount(inApp(client, createElement(TestClockBanner, { testClockId: CLOCK.id }), createElement(NowProbe, { record: { test_clock_id: CLOCK.id } })))
  await new Promise(r => setTimeout(r, 50))
  assert.equal(listReads, 0)
})

test('useNowFor keeps one value while the answer stays the same', async () => {
  vi.useFakeTimers({ toFake: ['Date'] })
  vi.setSystemTime(new Date('2026-11-20T10:00:00Z'))
  const values: number[] = []
  function Keyed({ record }: { record: ClockPinned }) {
    const now = useNowFor(record)
    useEffect(() => { values.push(now) }, [now])
    return null
  }
  const client = new QueryClient()
  page = await mount(inApp(client, createElement(Keyed, { record: { test_clock_id: null } })))
  vi.setSystemTime(new Date('2026-11-20T11:00:00Z'))
  // A new record object with the same answer, as a refetch gives.
  await page.render(inApp(client, createElement(Keyed, { record: { test_clock_id: null } })))
  assert.deepEqual(values, [Date.parse('2026-11-20T10:00:00Z')])
})

test('useNowResolver gives the real time of each render', async () => {
  vi.useFakeTimers({ toFake: ['Date'] })
  vi.setSystemTime(new Date('2026-11-20T10:00:00Z'))
  function Drawn() {
    return createElement('p', null, `drawn: ${new Date(useNowResolver()({ test_clock_id: null })).toISOString()}`)
  }
  const client = new QueryClient()
  page = await mount(inApp(client, createElement(Drawn)))
  vi.setSystemTime(new Date('2026-11-20T11:00:00Z'))
  await page.render(inApp(client, createElement(Drawn)))
  assert.match(page.text(), /drawn: 2026-11-20T11:00:00.000Z/)
})

test('an invoice page left open turns Past due when real time passes the due time', async () => {
  vi.useFakeTimers({ toFake: ['Date'] })
  vi.setSystemTime(new Date('2026-11-20T23:00:00Z'))
  const { default: InvoiceDetailPage } = await import('../src/pages/InvoiceDetail.tsx')
  const invoice = {
    id: 'hc_inv_1', invoice_number: 'INV-0001', customer_id: 'hc_cus_1', status: 'finalized', payment_status: 'pending',
    currency: 'USD', subtotal_cents: 2900, discount_cents: 0, tax_amount_cents: 0, tax_rate: 0, tax_name: '',
    total_amount_cents: 2900, amount_due_cents: 2900, amount_paid_cents: 0, credits_applied_cents: 0,
    billing_period_start: '2026-10-21T00:00:00Z', billing_period_end: '2026-11-21T00:00:00Z', billing_timezone: 'UTC',
    issued_at: '2026-11-11T00:00:00Z', due_at: '2026-11-21T00:00:00Z', created_at: '2026-11-11T00:00:00Z', is_simulated: false,
  }
  const client = new QueryClient({ defaultOptions: { queries: { staleTime: 30_000, retry: false } } })
  client.setQueryData(getGetTenantSettingsQueryKey(), { default_currency: 'USD', timezone: 'UTC' })
  client.setQueryData(['invoice', invoice.id], { invoice, line_items: [] })
  const tree = createElement(QueryClientProvider, { client },
    createElement(MemoryRouter, { initialEntries: [`/invoices/${invoice.id}`] },
      createElement(Routes, null, createElement(Route, { path: '/invoices/:id', element: createElement(InvoiceDetailPage) }))))
  page = await mount(tree)
  assert.match(page.text(), /Due in 1d/)
  assert.doesNotMatch(page.text(), /Past due/)

  // Midnight passes, and the page re-renders, as a refetch of the invoice makes it.
  vi.setSystemTime(new Date('2026-11-21T01:00:00Z'))
  client.setQueryData(['invoice', invoice.id], { invoice: { ...invoice, updated_at: '2026-11-21T01:00:00Z' }, line_items: [] })
  await until(() => assert.match(page!.text(), /Past due/))
})
