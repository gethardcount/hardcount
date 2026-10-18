import type { ReactNode } from 'react'

import { cn } from '@/lib/utils'

// Section is a titled block inside a page body: a card with a header row
// (title, optional description, actions on the right) and its content.
// `flush` removes the inner padding for tables and lists that run edge to
// edge. Use it instead of hand-built Card + CardHeader stacks so every
// section has the same spacing and title size.
export interface SectionProps {
  title?: ReactNode
  description?: ReactNode
  actions?: ReactNode
  flush?: boolean
  children: ReactNode
  className?: string
  id?: string
}

export function Section({ title, description, actions, flush, children, className, id }: SectionProps) {
  return (
    <section id={id} className={cn('rounded-xl bg-card text-card-foreground ring-1 ring-foreground/10 shadow-(--shadow-card)', className)}>
      {(title || actions) && (
        <div className="flex flex-wrap items-start justify-between gap-3 px-5 pt-4 pb-3">
          <div className="min-w-0">
            {title && <h2 className="text-sm font-semibold text-foreground">{title}</h2>}
            {description && <p className="mt-0.5 text-sm text-muted-foreground">{description}</p>}
          </div>
          {actions && <div className="flex shrink-0 flex-wrap items-center gap-2">{actions}</div>}
        </div>
      )}
      <div className={cn(flush ? 'pb-1' : 'px-5 pb-5', !(title || actions) && !flush && 'pt-5')}>{children}</div>
    </section>
  )
}
