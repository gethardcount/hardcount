import { useSyncExternalStore } from 'react'

// Below Tailwind's `sm` breakpoint (640px). Lists that stack each row on a
// phone render either the stacked row or the table columns, never both: a
// copy hidden with CSS still sits in the page, so a test or a find-in-page
// matched every status and amount twice.
const PHONE = '(max-width: 639.98px)'

function subscribe(onChange: () => void): () => void {
  const mq = window.matchMedia(PHONE)
  mq.addEventListener('change', onChange)
  return () => mq.removeEventListener('change', onChange)
}

export function useIsPhone(): boolean {
  return useSyncExternalStore(subscribe, () => window.matchMedia(PHONE).matches, () => false)
}
