import { Badge } from '@/components/ui/badge'
import { calendarDaysUntil } from '@/lib/dates'
import type { EffectiveNow } from '@/lib/effectiveNow'

interface DueBadgeProps {
  dueAt?: string | null
  warningDays?: number
  className?: string
  // The invoice's now, from useNowFor or useNowResolver, so an invoice on a
  // test clock reads relative to the clock's time. Engine actions (auto-charge
  // retries, dunning) fire on that time, so the badge has to match. While it
  // is not known (undefined), the badge is not drawn.
  now: EffectiveNow | undefined
}

// DueBadge renders an invoice's due-date status as the operator-
// facing terms the industry uses: "Past due", "Due today", "Due in N
// days". Unlike the generic ExpiryBadge, this component is scoped to
// invoice semantics — invoices don't expire, they go past-due.
//
// Stripe / Lago / Orb / Recurly / Chargebee all use "Past due". The
// previous implementation reused ExpiryBadge here and rendered
// "Expired", which suggests terminal state and confused operators
// looking at an Open invoice that's still actionable (collect, void,
// credit, email all remain valid past the due date).
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
