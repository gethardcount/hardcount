// @vitest-environment happy-dom
// After a clock change on the clock pages, an open page that reads the clock
// list (['test-clocks'], the one source of a clock's time) shows the change.
// The list kept the old time after an advance until its cache went stale,
// because the advance refreshed only the clock page's own read.
//
// The real pages are mounted, the operator clicks through the change, and a
// second reader of the list stands for any other open page. The cache keeps a
// read fresh for 30 seconds, as the app's does, so nothing refetches by
// itself.
import { createElement } from 'react'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { afterEach, beforeEach, test, vi } from 'vitest'
import assert from 'node:assert/strict'
import { api, setTenantTimezone, type TestClock } from '../src/lib/api.ts'
import { getGetTenantSettingsQueryKey } from '../src/lib/gen/queries.gen.ts'
import { useTestClocks } from '../src/hooks/useNow.ts'
import { click, mount, until, type Mounted } from './mount.ts'

vi.mock('@/contexts/AuthContext', () => ({
  useAuth: () => ({ user: { user_id: 'usr_1', tenant_id: 'ten_1', email: 'op@example.com', livemode: false }, loading: false }),
}))

setTenantTimezone('UTC')

// The server's clocks, which the changes move.
let server: TestClock[]

beforeEach(() => {
  server = [{ id: 'hc_clk_1', name: 'Renewal run', frozen_time: '2031-03-01T12:00:00Z', status: 'ready', created_at: '2031-01-01T00:00:00Z' } as TestClock]
  // Any other request stays pending, as a read still loading.
  vi.stubGlobal('fetch', () => new Promise(() => {}))
  vi.spyOn(api, 'listTestClocks').mockImplementation(async () => ({ data: server.map(c => ({ ...c })), total: server.length }) as never)
  vi.spyOn(api, 'getTestClock').mockImplementation(async id => ({ ...server.find(c => c.id === id)! }))
  vi.spyOn(api, 'listSubscriptionsOnClock').mockResolvedValue({ data: [], total: 0 } as never)
  vi.spyOn(api, 'listAttachedCustomers').mockResolvedValue({ data: [], total: 0 } as never)
})

let page: Mounted | undefined
afterEach(async () => {
  await page?.unmount()
  page = undefined
  vi.restoreAllMocks()
})

// OpenList is another open page reading the clock list.
function OpenList() {
  const { data } = useTestClocks()
  return createElement('ul', null, (data?.data ?? []).map(c => createElement('li', { key: c.id }, `list: ${c.name} at ${c.frozen_time}`)))
}

async function open(path: string): Promise<Mounted> {
  const { default: TestClocksPage } = await import('../src/pages/TestClocks.tsx')
  const { default: TestClockDetailPage } = await import('../src/pages/TestClockDetail.tsx')
  const client = new QueryClient({ defaultOptions: { queries: { staleTime: 30_000, retry: false } } })
  client.setQueryData(getGetTenantSettingsQueryKey(), { default_currency: 'USD', timezone: 'UTC' })
  page = await mount(createElement(QueryClientProvider, { client },
    createElement(MemoryRouter, { initialEntries: [path] },
      createElement(OpenList),
      createElement(Routes, null,
        createElement(Route, { path: '/test-clocks', element: createElement(TestClocksPage) }),
        createElement(Route, { path: '/test-clocks/:id', element: createElement(TestClockDetailPage) })))))
  return page
}

test('an advance shows the new time on every open page', async () => {
  const advance = vi.spyOn(api, 'advanceTestClock').mockImplementation(async (_id, to) => {
    server[0].frozen_time = to
    return { ...server[0] }
  })
  const p = await open('/test-clocks/hc_clk_1')
  await until(() => assert.match(p.text(), /list: Renewal run at 2031-03-01T12:00:00Z/))

  await click('Advance')
  await click('Advance clock') // the dialog's default target is an hour on

  assert.equal(advance.mock.calls.length, 1)
  await until(() => assert.match(p.text(), /list: Renewal run at 2031-03-01T13:00:00.000Z/))
})

test('a retried advance shows the new time on every open page', async () => {
  server[0].status = 'internal_failure'
  vi.spyOn(api, 'retryAdvanceTestClock').mockImplementation(async () => {
    Object.assign(server[0], { status: 'ready', frozen_time: '2031-04-01T12:00:00Z' })
    return { ...server[0] } as never
  })
  const p = await open('/test-clocks/hc_clk_1')
  await until(() => assert.match(p.text(), /list: Renewal run at 2031-03-01T12:00:00Z/))

  await click('Retry advance')

  await until(() => assert.match(p.text(), /list: Renewal run at 2031-04-01T12:00:00Z/))
})

test('a new clock shows on every open page', async () => {
  const create = vi.spyOn(api, 'createTestClock').mockImplementation(async ({ name, frozen_time }) => {
    const clock = { id: 'hc_clk_2', name, frozen_time, status: 'ready', created_at: frozen_time } as TestClock
    server.push(clock)
    return { ...clock }
  })
  const p = await open('/test-clocks')
  await until(() => assert.match(p.text(), /list: Renewal run/))

  await click('New test clock')
  const name = document.querySelector<HTMLInputElement>('input[name="name"]')!
  const { act } = await import('react')
  await act(async () => {
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!.call(name, 'Trial run')
    name.dispatchEvent(new Event('input', { bubbles: true }))
  })
  await click('Create test clock')
  await until(() => assert.equal(create.mock.calls.length, 1))

  await until(() => assert.match(p.text(), /list: Trial run at /))
})
