import type { ReactNode } from 'react'
import { Card, CardContent } from '@/components/ui/card'
import { DeltaBadge } from './DeltaBadge'

interface TrendCardProps {
  label: string
  value: ReactNode
  // Secondary hint line below the value (e.g. "2,345 invoices paid").
  hint?: ReactNode
  current?: number
  previous?: number
  // With both given, the card shows the change from the previous period.
  className?: string
  // What the number counts, shown as small text at the bottom of the card
  // (never only on hover, which touch screens and keyboards can't reach).
  definition?: string
}

// TrendCard is one of Home's cards: a label, a value, the change from the
// previous period when it has one, a hint line and what the number counts.
export function TrendCard({
  label,
  value,
  hint,
  current,
  previous,
  className,
  definition,
}: TrendCardProps) {
  const showDelta = typeof current === 'number' && typeof previous === 'number'

  return (
    <Card className={className}>
      <CardContent className="p-5">
        <p className="text-xs uppercase tracking-wider text-muted-foreground">{label}</p>
        <div className="flex items-baseline gap-2 mt-1">
          <p className="text-[28px] font-semibold tabular-nums leading-none text-foreground">{value}</p>
          {showDelta && (
            <DeltaBadge current={current!} previous={previous!} />
          )}
        </div>
        {hint && <p className="text-xs text-muted-foreground mt-1.5">{hint}</p>}
        {definition && <p className="text-xs text-muted-foreground mt-2">{definition}</p>}
      </CardContent>
    </Card>
  )
}
