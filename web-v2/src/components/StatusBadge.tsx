import { Badge } from '@/components/ui/badge'
import type { StatusDisplay, StatusTone } from '@/lib/statusLabels'

// StatusBadge renders a StatusDisplay from lib/statusLabels. Use it for new
// status pills so one state has one name and one colour app-wide.
const variant: Record<StatusTone, 'secondary' | 'info' | 'success' | 'warning' | 'danger'> = {
  neutral: 'secondary',
  info: 'info',
  success: 'success',
  warning: 'warning',
  danger: 'danger',
}

export function StatusBadge({ status, className }: { status: StatusDisplay; className?: string }) {
  return <Badge variant={variant[status.tone]} className={className}>{status.label}</Badge>
}
