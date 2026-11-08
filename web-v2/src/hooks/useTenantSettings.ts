import { useQuery, type QueryClient } from '@tanstack/react-query'
import { getGetTenantSettingsQueryOptions } from '@/lib/gen/queries.gen'
import type { TenantSettings } from '@/lib/gen/schemas/tenantSettings'
import { invalidateDefaultCurrencyFigures } from '@/lib/invalidateMoney'

// tenantSettingsQuery is the one query for GET /v1/settings, the account's
// settings (default currency, time zone, company details). Every page reads
// it, and a Settings save writes its answer into it, so the next page shows
// the saved values without another request.
export const tenantSettingsQuery = getGetTenantSettingsQueryOptions()

// useTenantSettings reads the account settings. A write that depends on one
// of them, such as the currency a new plan is created in, waits until they
// have loaded.
export function useTenantSettings() {
  return useQuery(tenantSettingsQuery)
}

// writeSavedSettings puts a Settings save's answer into the settings query of
// every client given: both modes' (useBothModeQueryClients), since test and
// live mode share the settings. When the default currency changed, it also
// marks stale the figures the server scopes to it.
export function writeSavedSettings(clients: readonly QueryClient[], saved: TenantSettings) {
  for (const client of clients) {
    const before = client.getQueryData(tenantSettingsQuery.queryKey)
    client.setQueryData(tenantSettingsQuery.queryKey, saved)
    if (before?.default_currency !== saved.default_currency) invalidateDefaultCurrencyFigures(client)
  }
}
