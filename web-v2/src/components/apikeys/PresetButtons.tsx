import { cn } from '@/lib/utils'

/** Offers a few fixed choices as a row of buttons and marks the picked one. */
export function PresetButtons<T extends string | number>({ presets, value, onChange }: {
  presets: readonly { value: T; label: string }[]
  value: T
  onChange: (value: T) => void
}) {
  return (
    <div className="flex flex-wrap gap-2">
      {presets.map(p => (
        <button
          key={p.value}
          type="button"
          aria-pressed={value === p.value}
          onClick={() => onChange(p.value)}
          className={cn(
            'px-3 py-1.5 rounded-lg text-sm border transition-colors',
            value === p.value
              ? 'border-primary bg-primary/5 text-primary font-medium'
              : 'border-border text-muted-foreground hover:text-foreground hover:border-border/80'
          )}
        >
          {p.label}
        </button>
      ))}
    </div>
  )
}
