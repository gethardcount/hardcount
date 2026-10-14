import { AlertCircle } from 'lucide-react'

import { Button } from '@/components/ui/button'
import { CopyButton } from '@/components/CopyButton'
import { errorRequestId, errorSentence, rawErrorDetail } from '@/lib/errorCopy'
import { cn } from '@/lib/utils'

// LoadError is what a page or card shows when its data didn't load. It must
// never look like "nothing here": an empty list after a failed request told
// operators they had no recipes, rates or prices when the request had failed.
//
// Title names what didn't load; the sentence comes from lib/errorCopy (the
// same words an error toast would use); Retry runs the load again. The raw
// error and a server fault's request id sit in a collapsed Details, so a
// screenshot sent to support still carries them.
export function LoadError({
  what,
  error,
  onRetry,
  className,
}: {
  // Fills "Couldn't load {what}", e.g. "recipes", "this plan".
  what: string
  error: unknown
  onRetry?: () => void
  className?: string
}) {
  const raw = rawErrorDetail(error)
  const requestId = errorRequestId(error)
  return (
    <div role="alert" className={cn('flex flex-col items-center px-6 py-10 text-center', className)}>
      <AlertCircle size={20} className="mb-2 text-destructive" aria-hidden />
      <p className="text-sm font-medium text-foreground">Couldn't load {what}</p>
      <p className="mt-1 max-w-md text-sm text-muted-foreground">{errorSentence(error)}</p>
      {onRetry && (
        <Button variant="outline" size="sm" className="mt-3" onClick={onRetry}>
          Retry
        </Button>
      )}
      {(raw || requestId) && (
        <details className="mt-3 max-w-md text-xs text-muted-foreground">
          <summary className="cursor-pointer hover:text-foreground">Details</summary>
          <div className="mt-2 space-y-1 text-left">
            {raw && <p className="break-words">{raw}</p>}
            {requestId && (
              <p className="flex items-center gap-1.5">
                Error ID: <span className="font-mono">{requestId}</span>
                <CopyButton text={requestId} title="Copy error ID" />
              </p>
            )}
          </div>
        </details>
      )}
    </div>
  )
}
