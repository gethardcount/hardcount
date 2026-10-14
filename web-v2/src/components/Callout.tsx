import type { ReactNode } from 'react'
import { AlertCircle, AlertTriangle, Info, type LucideIcon } from 'lucide-react'

import { CALLOUT_TONE_CLASSES, type CalloutTone } from '@/lib/calloutTone'
import { cn } from '@/lib/utils'

const DEFAULT_ICONS: Record<CalloutTone, LucideIcon> = {
  neutral: Info,
  amber: AlertTriangle,
  red: AlertCircle,
}

// Callout is the dashboard's one notice shape:
//   title (6 words or fewer) · one sentence · buttons · optional Details.
// The sentence says what happens and what to do. Anything longer, and any
// code or id that support might need, goes in Details, which starts
// collapsed. Never put the only way to act inside Details.
//
// Tone follows urgency, not the topic (see lib/calloutTone).
export function Callout({
  tone = 'neutral',
  icon,
  title,
  children,
  actions,
  details,
  detailsLabel = 'Details',
  footer,
  className,
  role,
}: {
  tone?: CalloutTone
  icon?: LucideIcon
  // Optional so a status line can be a single sentence.
  title?: ReactNode
  // The one sentence. Extra lines that depend on state may follow it.
  children?: ReactNode
  actions?: ReactNode
  details?: ReactNode
  detailsLabel?: string
  // A second, separately labelled disclosure under Details (the invoice
  // notice's "Provider response": Stripe's words, kept apart from Velox's).
  footer?: ReactNode
  className?: string
  role?: 'alert' | 'status'
}) {
  const Icon = icon ?? DEFAULT_ICONS[tone]
  const styles = CALLOUT_TONE_CLASSES[tone]
  return (
    <div role={role} className={cn('rounded-lg border px-4 py-3', styles.box, className)}>
      <div className="flex items-start gap-3">
        <Icon size={16} aria-hidden className={cn('mt-0.5 shrink-0', styles.icon)} />
        <div className="min-w-0 flex-1 space-y-1">
          {title && <p className="text-sm font-medium text-foreground">{title}</p>}
          {children && <div className="text-sm text-foreground/80 space-y-1">{children}</div>}
          {actions && <div className="flex flex-wrap items-center gap-2 pt-2">{actions}</div>}
          {details && (
            <details className="pt-1 text-xs text-muted-foreground">
              <summary className="cursor-pointer select-none hover:text-foreground">{detailsLabel}</summary>
              <div className="mt-2 space-y-1.5">{details}</div>
            </details>
          )}
          {footer}
        </div>
      </div>
    </div>
  )
}
