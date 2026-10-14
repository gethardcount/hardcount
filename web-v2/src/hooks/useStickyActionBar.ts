import { useEffect, useSyncExternalStore } from 'react'
import { showStickyActionBar, stickyActionBarShown, subscribeStickyActionBar } from '@/lib/stickyActionBar'

// useStickyActionBar marks a sticky action bar as shown while `shown` is true,
// so floating bottom-right UI (the onboarding pill) gets out of its way.
export function useStickyActionBar(shown: boolean): void {
  useEffect(() => {
    if (!shown) return
    return showStickyActionBar()
  }, [shown])
}

// useStickyActionBarShown is true while any page shows a sticky action bar.
export function useStickyActionBarShown(): boolean {
  return useSyncExternalStore(subscribeStickyActionBar, stickyActionBarShown, stickyActionBarShown)
}
