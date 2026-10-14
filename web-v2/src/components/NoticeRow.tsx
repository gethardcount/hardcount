import type { ReactNode } from 'react'
import { AlertCircle, AlertTriangle, Info, type LucideIcon } from 'lucide-react'

import { CALLOUT_TONE_CLASSES, type CalloutTone } from '@/lib/calloutTone'
import { cn } from '@/lib/utils'

const DEFAULT_ICONS: Record<CalloutTone, LucideIcon> = {
  neutral: Info,
  amber: AlertTriangle,
  red: AlertCircle,
}

// NoticeRow is a notice that is not the page's most urgent one. A page shows
// at most one notice box (Callout) above its content; any other notice that
// applies becomes this one line under the header: icon, title, sentence and
// its button, with no box. The icon keeps the tone so an amber state still
// reads as one. It wraps on a narrow screen instead of clipping.
export function NoticeRow({
  tone = 'neutral',
  icon,
  title,
  children,
  action,
  className,
}: {
  tone?: CalloutTone
  icon?: LucideIcon
  title: ReactNode
  children?: ReactNode
  action?: ReactNode
  className?: string
}) {
  const Icon = icon ?? DEFAULT_ICONS[tone]
  return (
    <div role="status" className={cn('flex flex-wrap items-center gap-x-2 gap-y-1 text-sm text-muted-foreground', className)}>
      <Icon size={14} aria-hidden className={cn('shrink-0', CALLOUT_TONE_CLASSES[tone].icon)} />
      <span className="min-w-0">
        <span className="font-medium text-foreground">{title}.</span>
        {children && <> {children}</>}
      </span>
      {action}
    </div>
  )
}
