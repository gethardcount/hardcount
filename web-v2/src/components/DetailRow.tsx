import type { ReactNode } from 'react'
import { cn } from '@/lib/utils'

// One label/value row in a detail card's divided list. On a phone the value
// sits under its label; from `sm` up it sits beside it, right-aligned. The
// value side can shrink (min-w-0) and long unbroken text (an email, an ID)
// wraps inside it instead of running over the label, which is what the
// customer page's Email row did at 390px.
export function DetailRow({
  label,
  labelClassName,
  children,
}: {
  label: ReactNode
  labelClassName?: string
  children: ReactNode
}) {
  return (
    <div className="flex flex-col items-start gap-1 px-6 py-3 sm:flex-row sm:items-center sm:justify-between sm:gap-4">
      <span className={cn('text-sm text-muted-foreground sm:shrink-0', labelClassName)}>{label}</span>
      <div className="flex min-w-0 max-w-full flex-wrap items-center gap-x-2 gap-y-1 text-sm text-foreground wrap-anywhere sm:justify-end sm:text-right">
        {children}
      </div>
    </div>
  )
}
