import type { ReactNode } from 'react'
import { Link } from 'react-router-dom'

import { cn } from '@/lib/utils'

// KpiStrip is a single row of headline numbers at the top of a page
// (MRR, outstanding, credit balance, next invoice). Each item can link to
// the list behind it. Tone colours the value only for states that need
// attention (danger for money owed past due, warning for at risk).
export interface Kpi {
  label: string
  value: ReactNode
  hint?: ReactNode
  tone?: 'default' | 'danger' | 'warning' | 'success'
  to?: string
}

const toneClass: Record<NonNullable<Kpi['tone']>, string> = {
  default: 'text-foreground',
  danger: 'text-danger-foreground',
  warning: 'text-warning-foreground',
  success: 'text-success-foreground',
}

const mdCols: Record<number, string> = { 1: 'md:grid-cols-1', 2: 'md:grid-cols-2', 3: 'md:grid-cols-3', 4: 'md:grid-cols-4' }

export function KpiStrip({ items, className }: { items: Kpi[]; className?: string }) {
  const n = Math.min(Math.max(items.length, 1), 4)
  return (
    <div className={cn('grid grid-cols-2 overflow-hidden rounded-xl bg-card ring-1 ring-foreground/10 shadow-(--shadow-card)', mdCols[n], className)}>
      {items.map((k, i) => {
        const body = (
          <>
            <p className="text-xs text-muted-foreground">{k.label}</p>
            <p className={cn('mt-1 text-xl font-semibold tabular-nums tracking-tight', toneClass[k.tone ?? 'default'])}>{k.value}</p>
            {k.hint && <p className="mt-0.5 text-xs text-muted-foreground">{k.hint}</p>}
          </>
        )
        const cls = cn(
          'min-w-0 px-5 py-4',
          i % 2 === 1 && 'border-l border-border',
          i >= 2 && 'border-t border-border md:border-t-0',
          i % n !== 0 && 'md:border-l',
        )
        return k.to ? (
          <Link key={k.label} to={k.to} className={cn(cls, 'transition-colors hover:bg-muted/40')}>{body}</Link>
        ) : (
          <div key={k.label} className={cls}>{body}</div>
        )
      })}
    </div>
  )
}
