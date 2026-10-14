import { toast } from 'sonner'

import type { Outcome } from './actionOutcomes'

// showOutcome renders an Outcome (lib/actionOutcomes) as one toast in its tone.
export function showOutcome(o: Outcome, action?: { label: string; onClick: () => void }): void {
  const opts = { description: o.description, action }
  switch (o.tone) {
    case 'success': toast.success(o.title, opts); break
    case 'warning': toast.warning(o.title, opts); break
    case 'error': toast.error(o.title, opts); break
    default: toast.info(o.title, opts)
  }
}
