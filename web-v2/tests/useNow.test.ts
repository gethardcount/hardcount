// What useNowFor and useNowResolver answer for each state of a record and its
// test clock. A record on no clock is at real time, and a record on a loaded
// clock is at the clock's time. Every "not known" row (the record or its
// clock has not loaded, the clock failed or is not in the list, live mode) is
// answered with real time for now: the hook is the one place that does so,
// and these rows say "real" until pages can hide a value that is not known.
//
// The hooks are rendered once on the server, from a cache that already holds
// the clock list or not. Server rendering sends no request, so a list missing
// from the cache is one still loading.
import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { test, vi } from 'vitest'
import assert from 'node:assert/strict'
import type { ClockPinned } from '../src/hooks/useNow.ts'

let livemode = false
vi.mock('@/contexts/AuthContext', () => ({
  useAuth: () => ({ user: { user_id: 'usr_1', tenant_id: 'ten_1', email: 'op@example.com', livemode } }),
}))

// The clock stands years from today, so a real-time answer cannot pass for it.
const CLOCK = { id: 'hc_clk_1', name: 'Renewal run', frozen_time: '2031-03-01T12:00:00Z', status: 'ready', created_at: '2031-01-01T00:00:00Z' }

type List = 'loaded' | 'loading' | 'failed'

interface Row {
  name: string
  record: ClockPinned | undefined
  list: List
  live?: boolean
  // 'clock' is the clock's time; 'real' is real time.
  want: 'clock' | 'real'
}

const ROWS: Row[] = [
  { name: 'record not loaded', record: undefined, list: 'loaded', want: 'real' },
  { name: 'record on no clock', record: { test_clock_id: undefined }, list: 'loaded', want: 'real' },
  { name: 'record on no clock, list not loaded', record: {}, list: 'loading', want: 'real' },
  { name: 'clock loading', record: { test_clock_id: CLOCK.id }, list: 'loading', want: 'real' },
  { name: 'clock failed to load', record: { test_clock_id: CLOCK.id }, list: 'failed', want: 'real' },
  { name: 'clock loaded', record: { test_clock_id: CLOCK.id }, list: 'loaded', want: 'clock' },
  { name: 'clock not in the list', record: { test_clock_id: 'hc_clk_gone' }, list: 'loaded', want: 'real' },
  { name: 'live mode', record: { test_clock_id: CLOCK.id }, list: 'loading', live: true, want: 'real' },
]

function cache(list: List): QueryClient {
  const client = new QueryClient({ defaultOptions: { queries: { retryOnMount: false } } })
  if (list === 'loaded') client.setQueryData(['test-clocks'], { data: [CLOCK], total: 1 })
  if (list === 'failed') {
    client.getQueryCache().build(client, { queryKey: ['test-clocks'] }).setState({
      status: 'error', fetchStatus: 'idle', error: new Error('500 Internal Server Error'),
      errorUpdatedAt: 1, errorUpdateCount: 1, fetchFailureCount: 2, fetchFailureReason: null,
    })
  }
  return client
}

// answer renders the hook and returns the "now" it gave, in epoch ms.
async function answer(row: Row, hook: 'useNowFor' | 'useNowResolver'): Promise<number> {
  const { useNowFor, useNowResolver } = await import('../src/hooks/useNow.ts')
  livemode = row.live ?? false
  function Probe() {
    const resolve = useNowResolver()
    const one = useNowFor(row.record)
    return String(hook === 'useNowFor' ? one : resolve(row.record))
  }
  return Number(renderToStaticMarkup(createElement(QueryClientProvider, { client: cache(row.list) }, createElement(Probe))))
}

for (const row of ROWS) {
  for (const hook of ['useNowFor', 'useNowResolver'] as const) {
    test(`${hook}: ${row.name} is ${row.want === 'clock' ? "the clock's time" : 'real time'}`, async () => {
      const before = Date.now()
      const now = await answer(row, hook)
      if (row.want === 'clock') {
        assert.equal(now, Date.parse(CLOCK.frozen_time))
      } else {
        assert.ok(now >= before && now <= Date.now(), `${new Date(now).toISOString()} is not real time`)
      }
    })
  }
}
