// After a clock advance, a page that is open and reads the clock list
// (['test-clocks'], the one source of a clock's time) shows the new time.
// The list kept the old time after an advance until its cache went stale,
// because the advance refreshed only the clock page's own read.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { QueryClient, QueryObserver } from '@tanstack/react-query'
import { refreshTestClocks } from '../src/hooks/useClockFrozenMap.ts'

interface Clock { id: string; frozen_time: string }

test('an advance refreshes the time an open list page shows', async () => {
  // The server's clock, which the advance moves.
  const server: Clock = { id: 'hc_clk_1', frozen_time: '2031-03-01T12:00:00Z' }
  const qc = new QueryClient({ defaultOptions: { queries: { staleTime: 30_000, retry: false } } })

  // An open page reading the list, as useTestClocks does.
  const list = new QueryObserver(qc, {
    queryKey: ['test-clocks'],
    queryFn: async () => ({ data: [{ ...server }], total: 1 }),
  })
  const unsubscribe = list.subscribe(() => {})
  await list.refetch()
  assert.equal(list.getCurrentResult().data?.data[0].frozen_time, '2031-03-01T12:00:00Z')

  // The clock page advances the clock, then refreshes what it changed.
  server.frozen_time = '2031-04-01T12:00:00Z'
  await refreshTestClocks(qc, server.id)

  assert.equal(list.getCurrentResult().data?.data[0].frozen_time, '2031-04-01T12:00:00Z')
  unsubscribe()
})
