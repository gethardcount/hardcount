import { FlaskConical } from 'lucide-react'
import { Link } from 'react-router-dom'

// SimulatedBadge marks a record whose dates follow a test clock's calendar,
// not today's: one visible word, "Simulated" (ADR-099: "test clock" is
// builder jargon, and raw clock ids stay out of visible copy). Placement
// follows the once-per-scope rule: mixed list rows and customer-scoped cards
// only; detail pages disclose through their banner.
//
// A <span> by default, never a <Link>: many parent rows are links, and an
// <a> inside an <a> is invalid HTML. With `link` and the clock's id (a
// customer-scoped card header, not inside an anchor) the chip opens the
// clock. Tooltip via the native title; the amber chip matches the test-mode
// banner.
export function SimulatedBadge({
  testClockId,
  link = false,
  title = "Dates follow a test clock's calendar, not today's date.",
  className = '',
}: {
  testClockId?: string
  link?: boolean
  title?: string
  className?: string
}) {
  const chipClass = `inline-flex shrink-0 items-center gap-1 rounded border border-amber-500/30 bg-amber-500/10 px-1.5 py-0.5 text-[10px] font-medium leading-none text-amber-800 dark:text-amber-300 ${className}`
  if (link && testClockId) {
    return (
      <Link
        to={`/test-clocks/${testClockId}`}
        aria-label="View test clock"
        title={`${title} Click to open the clock.`}
        className={`${chipClass} hover:border-amber-500/60 hover:bg-amber-500/20`}
      >
        <FlaskConical size={10} />
        Simulated
      </Link>
    )
  }
  return (
    <span title={title} className={chipClass}>
      <FlaskConical size={10} />
      Simulated
    </span>
  )
}
