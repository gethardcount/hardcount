// Home's "needs attention" lines. Each line's count is the total of the list
// it links to, read with the same filter, so the two never disagree (C-04).
// They used to come from the overview: a failed-payment count windowed to
// the period behind a link to every failed invoice.
//
// Dependency-free so the unit tests can load it.

import { outstandingText, type CurrencyAmount } from './money.ts'

export type AttentionSeverity = 'info' | 'warning' | 'danger'

export interface AttentionLine {
  severity: AttentionSeverity
  text: string
  to: string
}

// The lists each line counts and opens. A count is the list's `total` for
// this query with limit=1.
export const ATTENTION_LISTS = {
  failed: { query: 'status=finalized&payment_status=failed', to: '/invoices?status=finalized&payment_status=failed' },
  retrying: { query: 'state=active', to: '/dunning?state=active' },
  open: { query: 'status=finalized', to: '/invoices?status=finalized' },
} as const

export interface AttentionCounts {
  // undefined while loading or after a failed read: no line rather than 0.
  failed?: number
  retrying?: number
  open?: number
  // Refunds share their predicate with the credit-notes filter on the server
  // (domain.CreditNoteRefundNeedsAttentionSQL), so the overview's count is
  // the list's.
  refunds?: number
  // The overview's amount owed on the open invoices, per currency.
  outstanding?: readonly CurrencyAmount[]
}

const plural = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`

export function homeAttentionLines(c: AttentionCounts): AttentionLine[] {
  const lines: AttentionLine[] = []
  if (c.failed) {
    lines.push({
      severity: 'danger',
      text: `${plural(c.failed, 'open invoice', 'open invoices')} with a failed payment`,
      to: ATTENTION_LISTS.failed.to,
    })
  }
  if (c.retrying) {
    lines.push({
      severity: 'warning',
      text: `${plural(c.retrying, 'invoice', 'invoices')} in payment retry`,
      to: ATTENTION_LISTS.retrying.to,
    })
  }
  if (c.refunds) {
    lines.push({
      severity: 'danger',
      text: `${c.refunds} ${c.refunds === 1 ? 'refund needs' : 'refunds need'} attention`,
      to: '/credit-notes?refund_status=needs_attention',
    })
  }
  if (c.open) {
    lines.push({
      severity: 'info',
      text: outstandingText(c.open, c.outstanding ?? []),
      to: ATTENTION_LISTS.open.to,
    })
  }
  return lines
}
