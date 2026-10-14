// Colours for the dashboard's one notice shape (components/Callout). Three
// tones only, chosen by how urgent the state is:
//   neutral: information, nothing to do
//   amber:   needs action soon
//   red:     money at risk or something is blocked
// Every tone carries its own dark-mode classes; two hand-copied boxes on the
// subscription page once had none and rendered as bright light-mode panels in
// dark mode. Plain data so node --test can check it.

export type CalloutTone = 'neutral' | 'amber' | 'red'

export const CALLOUT_TONE_CLASSES: Record<CalloutTone, { box: string; icon: string }> = {
  neutral: {
    box: 'border-border bg-muted/40',
    icon: 'text-muted-foreground',
  },
  amber: {
    box: 'border-amber-300 bg-amber-50 dark:border-amber-500/30 dark:bg-amber-500/10',
    icon: 'text-amber-600 dark:text-amber-400',
  },
  red: {
    box: 'border-red-300 bg-red-50 dark:border-red-500/30 dark:bg-red-500/10',
    icon: 'text-red-600 dark:text-red-400',
  },
}

// toneForSeverity maps the server's attention severity onto a tone. An
// unknown severity reads as information, never as a warning.
export function toneForSeverity(severity: string | undefined): CalloutTone {
  if (severity === 'critical') return 'red'
  if (severity === 'warning') return 'amber'
  return 'neutral'
}
