import { cn } from '@/lib/utils'

interface HardcountLogoProps {
  size?: 'sm' | 'md' | 'lg'
  showText?: boolean
  className?: string
}

// The Hardcount mark: a tally of five. Four upright strokes in the current
// text colour and one copper stroke across them. It is the same drawing as
// the website's logo and web-v2/public/favicon.svg.
function HardcountMark({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 60 52" aria-hidden="true" className={cn('shrink-0', className)}>
      <g stroke="currentColor" strokeWidth="5" strokeLinecap="round">
        <line x1="10" y1="6" x2="10" y2="46" />
        <line x1="22" y1="6" x2="22" y2="46" />
        <line x1="34" y1="6" x2="34" y2="46" />
        <line x1="46" y1="6" x2="46" y2="46" />
      </g>
      <line
        x1="3" y1="40" x2="56" y2="12"
        strokeWidth="5" strokeLinecap="round"
        className="stroke-[#B4612A] dark:stroke-[#D9894F]"
      />
    </svg>
  )
}

export function HardcountLogo({ size = 'md', showText = true, className }: HardcountLogoProps) {
  const markSize = size === 'sm' ? 'h-5' : size === 'md' ? 'h-6' : 'h-8'
  const textSize = size === 'sm' ? 'text-base' : size === 'md' ? 'text-lg' : 'text-2xl'

  return (
    <div className={cn('flex items-center gap-2 text-foreground', className)}>
      <HardcountMark className={cn(markSize, 'w-auto')} />
      {showText && (
        <span className={cn('font-bold tracking-tight', textSize)}>Hardcount</span>
      )}
    </div>
  )
}
