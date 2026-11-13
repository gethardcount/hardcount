// What useNowFor and useNowResolver answer for each state of a record and its
// test clock. A record on no clock is at real time, and a record on a loaded
// clock is at the clock's time. Every other row is not known (undefined): the
// record or its clock has not loaded, the clock failed or is not in the list,
// or a live record carries a clock id. Real time is never the answer for a
// record on a clock.
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
  // 'clock' is the clock's time, 'real' is real time, 'unknown' is undefined.
  want: 'clock' | 'real' | 'unknown'
}

const ROWS: Row[] = [
  { name: 'record not loaded', record: undefined, list: 'loaded', want: 'unknown' },
  { name: 'record on no clock', record: { test_clock_id: undefined }, list: 'loaded', want: 'real' },
  { name: 'record on no clock, list not loaded', record: {}, list: 'loading', want: 'real' },
  { name: 'clock loading', record: { test_clock_id: CLOCK.id }, list: 'loading', want: 'unknown' },
  { name: 'clock failed to load', record: { test_clock_id: CLOCK.id }, list: 'failed', want: 'unknown' },
  { name: 'clock loaded', record: { test_clock_id: CLOCK.id }, list: 'loaded', want: 'clock' },
  { name: 'clock not in the list', record: { test_clock_id: 'hc_clk_gone' }, list: 'loaded', want: 'unknown' },
  { name: 'live record with a clock id', record: { test_clock_id: CLOCK.id }, list: 'loading', live: true, want: 'unknown' },
  { name: 'live record with a clock id, a test-mode list cached', record: { test_clock_id: CLOCK.id }, list: 'loaded', live: true, want: 'unknown' },
  { name: 'live record on no clock', record: {}, list: 'loading', live: true, want: 'real' },
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

// answer renders the hook and returns the "now" it gave, in epoch ms, or
// undefined when it gave none.
async function answer(row: Row, hook: 'useNowFor' | 'useNowResolver'): Promise<number | undefined> {
  const { useNowFor, useNowResolver } = await import('../src/hooks/useNow.ts')
  livemode = row.live ?? false
  function Probe() {
    const resolve = useNowResolver()
    const one = useNowFor(row.record)
    const now = hook === 'useNowFor' ? one : resolve(row.record)
    return now === undefined ? 'unknown' : String(now)
  }
  const out = renderToStaticMarkup(createElement(QueryClientProvider, { client: cache(row.list) }, createElement(Probe)))
  return out === 'unknown' ? undefined : Number(out)
}

for (const row of ROWS) {
  for (const hook of ['useNowFor', 'useNowResolver'] as const) {
    const said = { clock: "the clock's time", real: 'real time', unknown: 'not known' }[row.want]
    test(`${hook}: ${row.name} is ${said}`, async () => {
      const before = Date.now()
      const now = await answer(row, hook)
      if (row.want === 'clock') {
        assert.equal(now, Date.parse(CLOCK.frozen_time))
      } else if (row.want === 'unknown') {
        assert.equal(now, undefined)
      } else {
        assert.ok(now !== undefined)
        assert.ok(now >= before && now <= Date.now(), `${new Date(now).toISOString()} is not real time`)
      }
    })
  }
}
