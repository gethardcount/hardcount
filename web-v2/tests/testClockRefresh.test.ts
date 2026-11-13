// refreshTestClocks marks the clock list (['test-clocks'], the one source of a
// clock's time) stale, so a page that has it open reads it again and shows
// the clock's new time. The clock pages call it after every clock change.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { QueryClient, QueryObserver } from '@tanstack/react-query'
import { refreshTestClocks } from '../src/hooks/useNow.ts'

interface Clock { id: string; frozen_time: string }

test('refreshTestClocks refreshes an open list read', async () => {
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

  // The clock moves on the server, then the clock page refreshes the reads.
  server.frozen_time = '2031-04-01T12:00:00Z'
  await refreshTestClocks(qc)

  assert.equal(list.getCurrentResult().data?.data[0].frozen_time, '2031-04-01T12:00:00Z')
  unsubscribe()
})
