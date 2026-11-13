import { Card, CardContent } from '@/components/ui/card'
import { LoadError } from '@/components/LoadError'
import { useTestClocks, type ClockPinned } from '@/hooks/useNow'
import { cn } from '@/lib/utils'

/**
 * TestClocksLoadError is the load error for the clock list, which a page
 * shows once when the list failed to load and one of its records is on a
 * clock. Those records draw nothing counted from "now" until the list loads
 * (hooks/useNow), and Retry reads it again. A list that loaded once and then
 * failed to refresh still gives each clock its time, so it shows nothing.
 */
export function TestClocksLoadError({ records, className }: { records: readonly ClockPinned[]; className?: string }) {
  const { data, error, refetch } = useTestClocks()
  if (data || !error || !records.some(r => r.test_clock_id)) return null
  return (
    <Card className={cn('mb-4', className)}>
      <CardContent className="p-0">
        <LoadError what="test clocks" error={error} onRetry={() => { void refetch() }} className="py-6">
          Dates counted from a test clock&apos;s time appear once it loads.
        </LoadError>
      </CardContent>
    </Card>
  )
}
