import type { ReactNode } from 'react'
import { Card, CardContent } from '@/components/ui/card'
import { cn } from '@/lib/utils'
import { DeltaBadge } from './DeltaBadge'
import { Sparkline } from './Sparkline'

interface TrendCardProps {
  label: string
  value: ReactNode
  // Secondary hint line below the value (e.g. "2,345 invoices paid").
  hint?: ReactNode
  current?: number
  previous?: number
  // Delta direction semantics — if dropping is "good" (failed payments, churn)
  // set inverse so the badge color flips.
  inverseDelta?: boolean
  sparkline?: number[]
  sparklineTone?: 'primary' | 'success' | 'danger' | 'warning'
  // Tone of the value text (e.g. destructive when a counter > 0).
  valueTone?: 'default' | 'danger' | 'warning'
  className?: string
  // What the number counts, shown as small text at the bottom of the card
  // (never only on hover, which touch screens and keyboards can't reach).
  definition?: string
}

// TrendCard is the standard KPI card: label, value, period-over-period delta,
// optional trailing hint line and sparkline. Used across Dashboard + Analytics
// so the two pages speak the same visual vocabulary.
export function TrendCard({
  label,
  value,
  hint,
  current,
  previous,
  inverseDelta,
  sparkline,
  sparklineTone = 'primary',
  valueTone = 'default',
  className,
  definition,
}: TrendCardProps) {
  const showDelta = typeof current === 'number' && typeof previous === 'number'

  const valueColor =
    valueTone === 'danger' ? 'text-destructive'
    : valueTone === 'warning' ? 'text-amber-600 dark:text-amber-400'
    : 'text-foreground'

  return (
    <Card className={className}>
      <CardContent className="p-5">
        <p className="text-xs uppercase tracking-wider text-muted-foreground">{label}</p>
        <div className="flex items-baseline gap-2 mt-1">
          <p className={cn('text-[28px] font-semibold tabular-nums leading-none', valueColor)}>{value}</p>
          {showDelta && (
            <DeltaBadge current={current!} previous={previous!} inverse={inverseDelta} />
          )}
        </div>
        {hint && <p className="text-xs text-muted-foreground mt-1.5">{hint}</p>}
        {sparkline && sparkline.length > 1 && (
          <div className="mt-3">
            <Sparkline data={sparkline} tone={sparklineTone} ariaLabel={`${label} trend`} />
          </div>
        )}
        {definition && <p className="text-xs text-muted-foreground/80 mt-2">{definition}</p>}
      </CardContent>
    </Card>
  )
}
