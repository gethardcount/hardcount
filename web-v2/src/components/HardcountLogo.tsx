import { Zap } from 'lucide-react'
import { cn } from '@/lib/utils'

interface HardcountLogoProps {
  size?: 'sm' | 'md' | 'lg'
  showText?: boolean
  className?: string
}

export function HardcountLogo({ size = 'md', showText = true, className }: HardcountLogoProps) {
  const iconSize = size === 'sm' ? 16 : size === 'md' ? 20 : 24
  const boxSize = size === 'sm' ? 'w-8 h-8' : size === 'md' ? 'w-10 h-10' : 'w-12 h-12'
  const textSize = size === 'sm' ? 'text-base' : size === 'md' ? 'text-lg' : 'text-2xl'
  const radius = size === 'sm' ? 'rounded-lg' : 'rounded-xl'

  return (
    <div className={cn('flex items-center gap-2.5', className)}>
      <div className={cn('flex items-center justify-center bg-primary shrink-0', boxSize, radius)}>
        <Zap size={iconSize} className="text-primary-foreground" />
      </div>
      {showText && (
        <span className={cn('font-bold tracking-tight text-foreground', textSize)}>Hardcount</span>
      )}
    </div>
  )
}
