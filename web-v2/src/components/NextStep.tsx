import type { ReactNode } from 'react'
import { Link } from 'react-router-dom'
import { ArrowRight } from 'lucide-react'
import { cn } from '@/lib/utils'

/**
 * Offers the setup list's next step on the page where the step before it is
 * done: "Next: set your prices →". The page shows it only while that next step
 * is left (useOnboardingSteps().stepLeft).
 */
export function NextStep({ to, children, className }: { to: string; children: ReactNode; className?: string }) {
  return (
    <Link
      to={to}
      className={cn('inline-flex items-center gap-1 text-sm font-medium text-primary hover:underline', className)}
    >
      Next: {children}
      <ArrowRight size={14} aria-hidden />
    </Link>
  )
}
