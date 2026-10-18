import { useState, type ReactNode } from 'react'
import { Check, Copy } from 'lucide-react'

import { cn } from '@/lib/utils'

// PropertyList renders key/value facts in a rail or section: label on top,
// value under it, so long values (emails, IDs) never collide with labels in
// a 320px rail. `copy` adds a copy button for IDs and keys.
export interface Property {
  label: string
  value: ReactNode
  copy?: string
  mono?: boolean
  hidden?: boolean
}

export function PropertyList({ items, className }: { items: Property[]; className?: string }) {
  return (
    <dl className={cn('space-y-3', className)}>
      {items.filter(i => !i.hidden).map(i => (
        <div key={i.label} className="min-w-0">
          <dt className="text-xs text-muted-foreground">{i.label}</dt>
          <dd className={cn('mt-0.5 flex min-w-0 items-center gap-1.5 text-sm text-foreground', i.mono && 'font-mono text-xs')}>
            <span className="min-w-0 break-words">{i.value}</span>
            {i.copy && <CopyButton value={i.copy} label={i.label} />}
          </dd>
        </div>
      ))}
    </dl>
  )
}

function CopyButton({ value, label }: { value: string; label: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <button
      type="button"
      aria-label={`Copy ${label}`}
      className="shrink-0 rounded p-0.5 text-muted-foreground hover:bg-muted hover:text-foreground"
      onClick={() => {
        void navigator.clipboard?.writeText(value).then(() => {
          setCopied(true)
          setTimeout(() => setCopied(false), 1200)
        })
      }}
    >
      {copied ? <Check size={12} /> : <Copy size={12} />}
    </button>
  )
}
