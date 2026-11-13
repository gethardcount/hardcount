import { Link } from 'react-router-dom'
import { FlaskConical } from 'lucide-react'
import { formatDateTime } from '@/lib/api'
import { useTestClocks } from '@/hooks/useNow'
import { cn } from '@/lib/utils'

// TestClockBanner is the one line on a detail page whose record is pinned
// (directly or through its subscription) to a test clock. It is the only
// place on the page that says "simulated" (ADR-099); nothing lower on the
// page repeats it.
//
// One line, not a box (ADR-099 amendment, 2026-10-15): it states a fact
// about the page, not a problem, so it uses the neutral tone and stays out
// of the way of notices that need action. It shows the clock's current time
// so a "Past due" pill next to a future-looking due date makes sense. How
// simulated and real times appear on the page sits in a collapsed Details.
//
// The clock comes from the clock list (useTestClocks), the source every page
// reads a clock's time from, so the line shows the same time as the page.
export function TestClockBanner({ testClockId, className }: { testClockId: string; className?: string }) {
  const { data: clocks } = useTestClocks()
  const clock = clocks?.data.find(c => c.id === testClockId)

  // Deleting a clock deletes its records too, so this is rare (a page left
  // open while the clock was deleted). It stays as one line, without the
  // View-clock link, which would open a dead page.
  const clockDeleted = !!clocks && !clock

  return (
    <div className={cn('mb-6 text-sm text-muted-foreground', className)}>
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
        <FlaskConical size={14} aria-hidden className="shrink-0" />
        {clockDeleted ? (
          <span>
            <span className="font-medium text-foreground">Test clock deleted.</span>{' '}
            Dates on this page follow that clock&apos;s calendar, not today&apos;s date.
          </span>
        ) : (
          <>
            <span>
              <span className="font-medium text-foreground">Simulated time</span>
              {clock && <>: <span className="text-foreground">{formatDateTime(clock.frozen_time)}</span></>}.
              {' '}Dates on this page follow the test clock.
            </span>
            <Link
              to={`/test-clocks/${testClockId}`}
              className="text-primary underline-offset-2 hover:underline"
            >
              View clock
            </Link>
          </>
        )}
      </div>
      <details className="mt-1 pl-[22px] text-xs">
        <summary className="cursor-pointer select-none hover:text-foreground">Details</summary>
        <p className="mt-1.5 max-w-2xl">
          Anything that happens when the clock is advanced (an invoice being
          finalized, a payment retry, a reminder) is dated with the clock&apos;s
          time. When it also happened in the real world, such as an email being
          sent, a &quot;Recorded&quot; line under it shows the real time.
        </p>
      </details>
    </div>
  )
}
