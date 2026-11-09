import { useEffect, useSyncExternalStore, type ReactNode } from 'react'
import { LoadError } from '@/components/LoadError'
import { useTenantSettings } from '@/hooks/useTenantSettings'
import { getTenantTimezone, setTenantTimezone, subscribeTenantTimezone } from '@/lib/api'

// AccountSettingsGate draws a signed-in page only once the account settings
// (GET /v1/settings) have loaded. They hold the time zone that every picked
// day or time is sent in, and the default currency. A page drawn before
// would guess both: a credit expiring at a UTC midnight, a test clock set to
// the wrong hour. Until then it shows a wait, or the error with Retry.
//
// It also copies the zone into the display zone that formatDate and
// dates.ts read (setTenantTimezone), and draws the page only once that copy
// is in, so no date shows in the browser's zone first. The copy follows a
// Settings save, and is cleared when the gate goes, at sign-out.
export function AccountSettingsGate({ children }: { children: ReactNode }) {
  const settings = useTenantSettings()
  const zone = settings.data?.timezone
  useEffect(() => {
    if (zone) setTenantTimezone(zone)
  }, [zone])
  useEffect(() => () => setTenantTimezone(null), [])
  const displayZone = useSyncExternalStore(subscribeTenantTimezone, getTenantTimezone, getTenantTimezone)

  if (zone && displayZone) return children
  // A Retry that is loading shows the wait again.
  if (!zone && settings.isError && !settings.isFetching) {
    return (
      <LoadError what="your account settings" error={settings.error} onRetry={() => void settings.refetch()}>
        Every page needs the account's time zone and currency.
      </LoadError>
    )
  }
  return (
    <div role="status" aria-label="Loading your account settings" className="flex justify-center py-16">
      <div className="animate-spin h-8 w-8 border-2 border-primary border-t-transparent rounded-full" />
    </div>
  )
}
