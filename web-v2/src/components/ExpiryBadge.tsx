import { Badge } from '@/components/ui/badge'
import { daysUntil, type EffectiveNow } from '@/lib/effectiveNow'

interface ExpiryBadgeProps {
  expiresAt?: string | null
  warningDays?: number
  noExpiryLabel?: string
  label?: string
  className?: string
  // Reference "now" for the days-until calculation — REQUIRED. Take the
  // record's from useNowFor or useNowResolver, so a record on a test clock
  // reads "in N days" from the clock's time, or wallClockNow() for a genuinely
  // real-time surface (e.g. API-key expiry). Engine actions (dunning,
  // finalize) fire on simulation time; the badge has to match or it
  // understates urgency.
  now: EffectiveNow
}

export function ExpiryBadge({
  expiresAt,
  warningDays = 7,
  noExpiryLabel,
  label = 'Expires',
  className,
  now,
}: ExpiryBadgeProps) {
  if (!expiresAt) {
    return noExpiryLabel ? (
      <Badge variant="secondary" className={className}>{noExpiryLabel}</Badge>
    ) : null
  }

  const days = daysUntil(expiresAt, now)

  if (days < 0) {
    return <Badge variant="destructive" className={className}>Expired</Badge>
  }
  if (days <= warningDays) {
    return (
      <Badge variant="warning" className={className}>
        {days === 0 ? `${label} today` : `${label} in ${days}d`}
      </Badge>
    )
  }
  if (days <= 30) {
    return (
      <Badge variant="outline" className={`text-muted-foreground ${className ?? ''}`}>
        {label} in {days}d
      </Badge>
    )
  }
  return (
    <Badge variant="success" className={className}>
      {label} in {days}d
    </Badge>
  )
}
