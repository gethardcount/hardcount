import { useEffect } from 'react'
import { useQuery } from '@tanstack/react-query'
import { setTenantTimezone } from '@/lib/api'
import { useAuth } from '@/contexts/AuthContext'
import { tenantSettingsQuery } from '@/hooks/useTenantSettings'

// TenantTimezoneBootstrap is a render-nothing component that reads the
// account settings once the user is signed in and sets the module-scoped
// tenant timezone in lib/api.ts. formatDate and formatDateTime read that
// value, so every date on the dashboard shows in the tenant zone without
// passing it at each call. See ADR-010.
//
// It sits inside AuthProvider in main.tsx, so useAuth resolves, and mounts
// before sign-in. Its query therefore stays on the query client from before
// sign-in, a cache of its own: a Settings save does not reach it, so the save
// sets the module value itself (pages/Settings.tsx).
export function TenantTimezoneBootstrap() {
  const { user } = useAuth()
  const { data } = useQuery({ ...tenantSettingsQuery, enabled: !!user })

  useEffect(() => {
    if (data?.timezone) {
      setTenantTimezone(data.timezone)
    } else if (!user) {
      // Logout — reset to browser-local fallback so a subsequent
      // login from a different tenant doesn't render the prior
      // tenant's TZ.
      setTenantTimezone(null)
    }
  }, [data?.timezone, user])

  return null
}
