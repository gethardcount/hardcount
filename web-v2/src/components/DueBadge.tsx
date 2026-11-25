import { Badge } from '@/components/ui/badge'
import { calendarDaysUntil } from '@/lib/dates'
import type { EffectiveNow } from '@/lib/effectiveNow'

interface DueBadgeProps {
  dueAt?: string | null
  warningDays?: number
  className?: string
  // The invoice's now, from useNowFor or useNowResolver. While it is not
  // known (undefined), the badge is not drawn.
  now: EffectiveNow | undefined
}

// DueBadge renders an invoice's due-date status in the operator-facing
// terms the industry uses (Stripe, Lago, Orb, Recurly, Chargebee): "Past
// due", "Due today", "Due in N days". An invoice does not expire, it goes
// past due: "Expired" would suggest a terminal state, while collect, void,
// credit and email all remain valid on an open invoice past its due date.
export function DueBadge({ dueAt, warningDays = 3, className, now }: DueBadgeProps) {
  if (!dueAt || now === undefined) return null

  // Calendar days in tenant TZ, the zone the Due date beside the badge is
  // printed in (formatDate). The deadline is a date, so on the due date the
  // badge reads "Due today" all day, and "Past due 1d" from the next day.
  const days = calendarDaysUntil(dueAt, now)

  if (days < 0) {
    const overdue = Math.abs(days)
    return (
      <Badge variant="destructive" className={className}>
        {overdue === 1 ? 'Past due 1d' : `Past due ${overdue}d`}
      </Badge>
    )
  }
  if (days === 0) {
    return <Badge variant="warning" className={className}>Due today</Badge>
  }
  if (days <= warningDays) {
    return <Badge variant="warning" className={className}>Due in {days}d</Badge>
  }
  if (days <= 30) {
    return (
      <Badge variant="outline" className={`text-muted-foreground ${className ?? ''}`}>
        Due in {days}d
      </Badge>
    )
  }
  return (
    <Badge variant="success" className={className}>
      Due in {days}d
    </Badge>
  )
}
