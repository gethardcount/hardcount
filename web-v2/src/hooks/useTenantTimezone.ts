import { getTenantTimezone } from '@/lib/api'
import { useTenantSettings } from '@/hooks/useTenantSettings'

// useTenantTimezone returns the tenant timezone as React state, so a memo that
// turns civil dates into UTC instants can list it as a dependency and recompute
// once /v1/settings arrives. Reading the module-level getTenantTimezone() inside
// a memo does not do that: a cold load renders before settings land, the memo
// caches UTC day bounds, and nothing invalidates it. It reads the one settings
// query, which a Settings save updates. Falls back to the module value, then
// 'UTC' (the DB default), until settings load.
export function useTenantTimezone(): string {
  const { data } = useTenantSettings()
  return data?.timezone || getTenantTimezone() || 'UTC'
}
