import type { ReactNode } from 'react'

import { cn } from '@/lib/utils'

// DetailLayout is the body of a detail page: the main column (what the
// operator came to see and act on) and a right rail of reference details
// (IDs, settings, contact). Below the lg breakpoint the rail stacks under
// the main column. Borrowed from the Stripe customer and invoice pages.
export function DetailLayout({ main, rail, className }: { main: ReactNode; rail?: ReactNode; className?: string }) {
  if (!rail) return <div className={cn('space-y-6', className)}>{main}</div>
  return (
    <div className={cn('grid grid-cols-1 gap-6 lg:grid-cols-[minmax(0,1fr)_320px]', className)}>
      <div className="min-w-0 space-y-6">{main}</div>
      <aside className="min-w-0 space-y-6">{rail}</aside>
    </div>
  )
}
