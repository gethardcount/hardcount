import { createContext, useContext } from 'react'
import { useQueryClient, type QueryClient } from '@tanstack/react-query'

// The query clients of test and live mode, from ModeAwareQueryProvider. Each
// mode has its own cache, and the cache of the mode not open is kept for the
// next switch.
export const ModeQueryClientsContext = createContext<readonly QueryClient[] | null>(null)

// useBothModeQueryClients returns the query client of each mode. Data that
// both modes share, such as the account settings, is written into both, so
// a switch to the other mode never shows an older copy. Outside the provider
// it is the one client in use.
export function useBothModeQueryClients(): readonly QueryClient[] {
  const active = useQueryClient()
  return useContext(ModeQueryClientsContext) ?? [active]
}
