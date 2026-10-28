import { useEffect } from 'react'

// usePageTitle sets the browser-tab title for the current page —
// "Invoices · Hardcount", "INV-2026-0042 · Hardcount" — so multi-tab operator
// workflows are navigable (every tab previously read the Vite default).
// Pass the entity label once loaded; undefined falls back to the app name.
export function usePageTitle(title?: string) {
  useEffect(() => {
    document.title = title ? `${title} · Hardcount` : 'Hardcount'
    return () => {
      document.title = 'Hardcount'
    }
  }, [title])
}
