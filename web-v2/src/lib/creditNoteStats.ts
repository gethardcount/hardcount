// The Credit Notes page's header, tabs and stat cards, worded from
// GET /v1/credit-notes/stats.
//
// They used to be computed from the rows the page had loaded: at most 100
// notes, and only the open tab's status, so past 100 notes every figure was
// short and under a status tab the other tabs' counts disappeared (E12). The
// server now counts every note and sums money per currency. It also decides
// what "refunded" means: only refund legs that succeeded, because a refund
// amount on a note is an instruction to move cash and refund_status says
// whether it moved (observed live 2026-08-03: $30.00 of failed legs shown as
// money returned to cards).
//
// Contrast creditNoteCeilings.ts, which counts failed legs on purpose: the
// over-refund ceiling mirrors the server cap and errs toward counting; a
// "money returned" figure must be true.

import { amountsByCurrencyText, formatCents } from './money'
import type { CreditNoteStats } from './gen/schemas/creditNoteStats'
import type { CreditNoteStatsByCurrencyItem } from './gen/schemas/creditNoteStatsByCurrencyItem'

export type { CreditNoteStats }

type MoneyField = Exclude<keyof CreditNoteStatsByCurrencyItem, 'currency'>

export interface CreditNoteStatFigures {
  counts: { all: number; draft: number; issued: number; voided: number }
  total: string
  /** Succeeded refund legs only: cash that went back. */
  refunded: string
  /** Refund legs not confirmed yet (waiting on Stripe, an unclear answer, or
   * a result never recorded), or null when there are none. */
  refundPending: string | null
  /** Refund legs that failed, or null when there are none. */
  refundFailed: string | null
  credited: string
}

// creditNoteStatFigures words the server's totals for the page. Amounts in
// several currencies are shown side by side ("$10.00 + €7.00"), never added.
export function creditNoteStatFigures(stats: CreditNoteStats, defaultCurrency: string): CreditNoteStatFigures {
  const text = (field: MoneyField) =>
    amountsByCurrencyText(stats.by_currency.map(c => ({ currency: c.currency, amount_cents: c[field] })))
  const money = (field: MoneyField) => text(field) ?? formatCents(0, defaultCurrency)
  return {
    counts: {
      all: stats.draft_count + stats.issued_count + stats.voided_count,
      draft: stats.draft_count,
      issued: stats.issued_count,
      voided: stats.voided_count,
    },
    total: money('total_cents'),
    refunded: money('refunded_cents'),
    refundPending: text('refund_pending_cents'),
    refundFailed: text('refund_failed_cents'),
    credited: money('credited_cents'),
  }
}

export interface CreditNotesListView {
  /** Tab counts, or null under the refund filter: the server counts notes by
   * status only, so there they would count notes the list does not show. */
  tabCounts: CreditNoteStatFigures['counts'] | null
  /** What an empty list says under a filter, and which filter Clear removes.
   * Null with no filter: the page shows its first-use state. */
  empty: { title: string; description: string; clear: 'status' | 'refund_status' } | null
}

// creditNotesListView decides what the tabs and an empty list say for the
// open filters. The Dashboard links to the refund filter; there the tab
// counts read "Issued 200" over 3 rows, and an empty list said "No credit
// notes yet" under "250 total".
export function creditNotesListView(
  counts: CreditNoteStatFigures['counts'] | undefined,
  status: string,
  refundStatus: string,
): CreditNotesListView {
  if (refundStatus) {
    return {
      tabCounts: null,
      empty: refundStatus === 'needs_attention'
        ? {
            title: 'No refunds need attention',
            description: 'No refund has failed, is unconfirmed, or has been pending for more than 72 hours.',
            clear: 'refund_status',
          }
        : { title: 'No credit notes with this refund status', description: 'Clear the filter to see all credit notes.', clear: 'refund_status' },
    }
  }
  return {
    tabCounts: counts ?? null,
    empty: status
      ? { title: `No ${status} credit notes`, description: 'Try a different filter to see more results.', clear: 'status' }
      : null,
  }
}
