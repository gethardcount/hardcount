import { useQuery } from '@tanstack/react-query'
import { api, getTenantTimezone } from '@/lib/api'

// useTenantTimezone returns the tenant timezone as React state, so a memo that
// turns civil dates into UTC instants can list it as a dependency and recompute
// once /v1/settings arrives. Reading the module-level getTenantTimezone() inside
// a memo does not do that: a cold load renders before settings land, the memo
// caches UTC day bounds, and nothing invalidates it. The ['settings'] query is
// shared with TenantTimezoneBootstrap, so this adds no request. Falls back to
// the module value, then 'UTC' (the DB default), until settings load.
export function useTenantTimezone(): string {
  const { data } = useQuery({
    queryKey: ['settings'],
    queryFn: () => api.getSettings(),
    staleTime: 5 * 60 * 1000,
  })
  return data?.timezone || getTenantTimezone() || 'UTC'
}
