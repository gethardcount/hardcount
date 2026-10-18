import type { ReactNode } from 'react'
import { Link } from 'react-router-dom'
import { ArrowLeft } from 'lucide-react'

import { cn } from '@/lib/utils'

// PageHeader is the top of every page: an optional back link, the title with
// its status badges beside it, one line of key facts (meta), and the page's
// actions on the right. Rule: at most one primary (filled) button in
// `actions`; everything else is outline, ghost or in an overflow menu.
//
//   ← Customers
//   Globex Corporation  [Active] [Simulated]        [New invoice] [⋯]
//   globex · finance@globex.test · since Jul 13, 2026
export interface PageHeaderProps {
  title: ReactNode
  // Back link to the parent list ("← Customers"). Detail pages set it.
  back?: { to: string; label: string }
  // Badges rendered right after the title (status, simulated, archived).
  badges?: ReactNode
  // One muted line of key facts under the title.
  meta?: ReactNode
  // Longer explanation for list pages ("Every invoice, newest first.").
  description?: ReactNode
  actions?: ReactNode
  className?: string
}

export function PageHeader({ title, back, badges, meta, description, actions, className }: PageHeaderProps) {
  return (
    <header className={cn('mb-6', className)}>
      {back && (
        <Link
          to={back.to}
          className="mb-3 inline-flex items-center gap-1 text-sm text-muted-foreground transition-colors hover:text-foreground"
        >
          <ArrowLeft size={14} aria-hidden />
          {back.label}
        </Link>
      )}
      <div className="flex flex-wrap items-start justify-between gap-x-6 gap-y-3">
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <h1 className="truncate text-2xl font-semibold tracking-tight text-foreground">{title}</h1>
            {badges}
          </div>
          {meta && <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-1 text-sm text-muted-foreground">{meta}</div>}
          {description && <p className="mt-1 max-w-3xl text-sm text-muted-foreground">{description}</p>}
        </div>
        {actions && <div className="flex shrink-0 flex-wrap items-center gap-2">{actions}</div>}
      </div>
    </header>
  )
}

// MetaSep separates facts in PageHeader.meta ("globex · Net 30 · USD").
export function MetaSep() {
  return <span aria-hidden className="text-muted-foreground/50">·</span>
}
