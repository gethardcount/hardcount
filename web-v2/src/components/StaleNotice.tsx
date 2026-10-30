import { RefreshCw } from 'lucide-react'

import { Button } from '@/components/ui/button'

// StaleNotice is what a page shows when a refresh fails after it had
// loaded: one line above the page, which keeps what it showed. Swapping the
// whole page for an error screen lost any open dialog and what was typed in
// it, during what was usually a short outage (B-06). A page that never
// loaded shows LoadError instead.
export function StaleNotice({ what, onRetry }: { what: string; onRetry: () => void }) {
  return (
    <div role="status" className="mb-4 flex flex-wrap items-center gap-x-3 gap-y-1 rounded-lg border border-border bg-muted px-4 py-2 text-sm text-foreground">
      <RefreshCw size={14} className="text-muted-foreground" aria-hidden />
      <span>Couldn't refresh {what}. This is what was loaded before.</span>
      <Button variant="outline" size="xs" onClick={onRetry}>Retry</Button>
    </div>
  )
}
