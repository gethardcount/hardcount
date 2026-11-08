import { useQuery } from '@tanstack/react-query'
import { getGetTenantSettingsQueryOptions } from '@/lib/gen/queries.gen'

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
