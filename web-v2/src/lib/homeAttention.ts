// Home's "needs attention" lines. Each invoice line's count is the total of
// the list it links to, read with the same filter, so the two never disagree
// (C-04). The credit lines count customers, from the overview, with the
// predicate of the Credit list's filter they link to.
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
  // Customers with a balance in some currency at zero or less, and above zero
  // but below the low-balance setting (0 when it is unset). The server shares
  // the predicate with the Credit list's filter
  // (domain.CreditBalanceFilterSQL); the list has a row per customer and
  // currency, the count one per customer.
  outOfCredit?: number
  lowOnCredit?: number
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
  if (c.outOfCredit) {
    lines.push({
      severity: 'danger',
      text: `${plural(c.outOfCredit, 'customer is', 'customers are')} out of credit`,
      to: '/credits?balance=out',
    })
  }
  if (c.lowOnCredit) {
    lines.push({
      severity: 'warning',
      text: `${plural(c.lowOnCredit, 'customer is', 'customers are')} low on credit`,
      to: '/credits?balance=low',
    })
  }
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
