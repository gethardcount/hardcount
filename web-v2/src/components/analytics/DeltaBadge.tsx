import { ArrowDown, ArrowUp, Minus } from 'lucide-react'
import { cn } from '@/lib/utils'

interface DeltaBadgeProps {
  current: number
  previous: number
  className?: string
}

// DeltaBadge renders a compact percent-change indicator vs. a prior period.
// Accessible label includes direction and magnitude; color is supplementary.
export function DeltaBadge({ current, previous, className }: DeltaBadgeProps) {
  // Choose an appropriate sentinel when the prior period was zero.
  let pct: number | null = null
  if (previous === 0 && current === 0) pct = 0
  else if (previous === 0) pct = null
  else pct = ((current - previous) / Math.abs(previous)) * 100

  const direction = pct === null ? 'n/a' : pct > 0 ? 'up' : pct < 0 ? 'down' : 'flat'
  const color = direction === 'up'
    ? 'text-success-foreground'
    : direction === 'down'
      ? 'text-destructive'
      : 'text-muted-foreground'

  const label = pct === null
    ? 'no prior data'
    : `${Math.abs(pct).toFixed(Math.abs(pct) < 10 ? 1 : 0)}% ${direction === 'up' ? 'higher' : direction === 'down' ? 'lower' : 'unchanged'} than prior period`

  return (
    <span
      className={cn('inline-flex items-center gap-0.5 text-xs font-medium tabular-nums', color, className)}
      aria-label={label}
      title={label}
    >
      {pct === null ? (
        <span className="text-muted-foreground">—</span>
      ) : direction === 'up' ? (
        <ArrowUp size={12} aria-hidden />
      ) : direction === 'down' ? (
        <ArrowDown size={12} aria-hidden />
      ) : (
        <Minus size={12} aria-hidden />
      )}
      {pct !== null && <span>{Math.abs(pct).toFixed(Math.abs(pct) < 10 ? 1 : 0)}%</span>}
    </span>
  )
}
