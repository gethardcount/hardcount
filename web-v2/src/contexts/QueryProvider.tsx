import { useMemo, type ReactNode } from 'react'
import { QueryCache, QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { useAuth } from '@/contexts/AuthContext'
import { retryQuery, scheduleRateLimitRefetch } from '@/lib/rateLimitRetry'

// Refetch posture (2026-06-02): operator billing data changes on the order of
// minutes and is refreshed explicitly via invalidateQueries after mutations,
// so aggressive time/focus-based refetching just churns the single HTTP/1.1
// dev origin (and prod) with duplicate global queries. staleTime=30s lets
// normal page-to-page navigation serve from cache; refetchOnWindowFocus=false
// stops a full re-fetch on every alt-tab. Real-time surfaces (test-clock
// advancing, invoice payment status) opt into polling via per-query
// refetchInterval, which is independent of these defaults.
//
// retry: one retry for a failed load, except a 429 (Too many requests). A
// query that is retrying stays pending, so the page showed its skeleton
// until the retries ran out. A 429 now shows the error state at once, and
// the query loads again after the API's Retry-After (lib/rateLimitRetry).
const queryDefaults = {
  queries: {
    staleTime: 30_000,
    retry: retryQuery,
    refetchOnWindowFocus: false,
    refetchOnMount: true,
  },
} as const

function newQueryClient(): QueryClient {
  const client: QueryClient = new QueryClient({
    queryCache: new QueryCache({
      onError: (err, query) => {
        scheduleRateLimitRefetch(err, query, () => {
          void client.refetchQueries({ queryKey: query.queryKey, exact: true, type: 'active' })
        })
      },
    }),
    defaultOptions: queryDefaults,
  })
  return client
}

// Mode-scoped data (customers, invoices, subscriptions, API keys, audit
// log entries, etc.) lives in fully separate caches per mode. Toggling
// Test↔Live swaps the active QueryClient and remounts the route tree so
// already-subscribed useQuery hooks rebind to the new client. Going back
// the other way reuses the prior cache (the QueryClient instance is
// retained across the remount), so back-and-forth toggling renders
// instantly from cache before any background refetch.
//
// Why the remount: React Query's QueryObserver is constructed once per
// useQuery mount via useState(() => new QueryObserver(client, opts))
// and captures the client reference at construction time. Swapping the
// client via QueryClientProvider context updates the context value but
// doesn't move existing observers — they stay bound to the old client
// and keep returning the old cache. Forcing a key-driven remount is the
// idiomatic React Query escape hatch (per the maintainers' guidance for
// "swap clients dynamically" use cases).
//
// Per-user-id memo recreates fresh clients on logout / different
// sign-in so the previous user's cache is gc'd. Anonymous bucket while
// auth loads keeps the signed-out routes (such as /login and
// /invoice/:token) functional.
export function ModeAwareQueryProvider({ children }: { children: ReactNode }) {
  const { user } = useAuth()
  const userKey = user ? `${user.tenant_id}:${user.user_id}` : 'anon'

  const { testClient, liveClient } = useMemo(
    () => ({
      testClient: newQueryClient(),
      liveClient: newQueryClient(),
    }),
    [userKey],
  )

  const livemode = user?.livemode ?? false
  const active = livemode ? liveClient : testClient
  // key={mode} forces the provider subtree to unmount + remount on
  // every mode flip. Component state (filters, expanded rows, modal
  // open) resets — correct for mode toggle since those are mode-scoped
  // contexts. The QueryClient instances themselves are stored in the
  // parent's useMemo, so caches survive the remount.
  return (
    <QueryClientProvider client={active} key={livemode ? 'live' : 'test'}>
      {children}
    </QueryClientProvider>
  )
}
