// How the customer page explains a credit balance.
//
// A $500 grant that was fully applied to an invoice left no trace on the
// customer page: the balance read $0.00, the grants card listed only grants
// with something left, and Outstanding ($4,160) disagreed with the invoice
// total ($4,660) with no word about the $500 of credit between them. These
// helpers give the page the totals, the used-up grants and that sentence.
import { formatCents } from './money.ts'

export interface CreditTotals {
  balance_cents: number
  total_granted: number
  total_used: number
  total_expired: number
  // Commit credit taken back with its voided or refunded invoice, and credit
  // an operator deducted.
  total_retired?: number
  total_deducted?: number
}

// creditTotalsLine is "Granted $500.00 · Used $500.00", plus "· Expired $x",
// "· Canceled $x" and "· Deducted $x" for the parts that are above zero, so
// Granted = Used + Expired + Canceled + Deducted + the balance shown above
// it. Null when nothing was ever granted.
export function creditTotalsLine(t: CreditTotals, currency: string): string | null {
  if (t.total_granted <= 0) return null
  const parts = [`Granted ${formatCents(t.total_granted, currency)}`, `Used ${formatCents(t.total_used, currency)}`]
  if (t.total_expired > 0) parts.push(`Expired ${formatCents(t.total_expired, currency)}`)
  if ((t.total_retired ?? 0) > 0) parts.push(`Canceled ${formatCents(t.total_retired ?? 0, currency)}`)
  if ((t.total_deducted ?? 0) > 0) parts.push(`Deducted ${formatCents(t.total_deducted ?? 0, currency)}`)
  return parts.join(' · ')
}

export interface GrantRow {
  amount_cents: number
  consumed_cents: number
  remaining_cents: number
  // consumed_cents also counts credit no invoice used, which the server
  // names: expired_cents, what ran out of time unused (the expiry sweep sets
  // consumed to the full amount); retired_cents, commit credit taken back
  // when its invoice was voided or refunded by a credit note; and
  // deducted_cents, what an operator deducted from this grant.
  expired_cents?: number
  retired_cents?: number
  deducted_cents?: number
  expires_at?: string
}

export type GrantState = 'active' | 'used_up' | 'expired' | 'canceled' | 'deducted'

// grantState: a grant with nothing left is "expired" when some of it ran out
// of time unused, "canceled" when the rest was taken back with its invoice
// (voided, or refunded by a credit note), "deducted" when an operator
// deducted all of it, and "used up" when invoices drew some of it. Expiry
// wins over canceled: the sweep takes whatever is left, so it is the last
// thing that can happen to a grant. A grant past its expiry that the sweep
// has not written off yet is "expired" too.
export function grantState(g: GrantRow, nowMs: number): GrantState {
  if (g.remaining_cents > 0) {
    return g.expires_at && Date.parse(g.expires_at) <= nowMs ? 'expired' : 'active'
  }
  if ((g.expired_cents ?? 0) > 0) return 'expired'
  if ((g.retired_cents ?? 0) > 0) return 'canceled'
  if ((g.deducted_cents ?? 0) > 0 && grantUsedCents(g) <= 0) return 'deducted'
  return 'used_up'
}

// grantUsedCents is what invoices really used: consumed less the expired,
// canceled and deducted parts.
export function grantUsedCents(g: GrantRow): number {
  return g.consumed_cents - (g.expired_cents ?? 0) - (g.retired_cents ?? 0) - (g.deducted_cents ?? 0)
}

// splitGrants keeps the order the server sent and separates the grants that
// still hold credit from the finished ones (shown behind a toggle).
export function splitGrants<T extends GrantRow>(grants: readonly T[], nowMs: number): { active: T[]; finished: T[] } {
  const active: T[] = []
  const finished: T[] = []
  for (const g of grants) (grantState(g, nowMs) === 'active' ? active : finished).push(g)
  return { active, finished }
}

// invoiceCreditNote explains why an invoice's amount due is below its total:
// "$500.00 credit applied · $4,160.00 due". Null when no credit was applied.
export function invoiceCreditNote(inv: {
  credits_applied_cents?: number
  amount_due_cents: number
  currency: string
}): string | null {
  const applied = inv.credits_applied_cents ?? 0
  if (applied <= 0) return null
  return `${formatCents(applied, inv.currency)} credit applied · ${formatCents(inv.amount_due_cents, inv.currency)} due`
}

// outstandingIncludesCredit: does any invoice counted in Outstanding have
// credit applied? Outstanding counts the same invoices the server does
// (GetOutstandingBalance): unpaid (pending, failed, unknown) and not draft,
// voided or uncollectible. A paid invoice that had credit says nothing about
// what is owed now.
const UNPAID = new Set(['pending', 'failed', 'unknown'])
const NOT_OWED = new Set(['draft', 'voided', 'uncollectible'])
export function outstandingIncludesCredit(invoices: readonly {
  status: string
  payment_status: string
  credits_applied_cents?: number
}[]): boolean {
  return invoices.some(inv => UNPAID.has(inv.payment_status) && !NOT_OWED.has(inv.status) && (inv.credits_applied_cents ?? 0) > 0)
}

// ledgerEntryLabel names a ledger entry type in plain words. "usage" is the
// ledger's word for credit applied to an invoice.
export function ledgerEntryLabel(entryType: string): string {
  switch (entryType) {
    case 'grant': return 'Grant'
    case 'usage': return 'Applied'
    case 'expiry': return 'Expired'
    case 'adjustment': return 'Adjustment'
    default: return entryType ? entryType[0].toUpperCase() + entryType.slice(1).replace(/_/g, ' ') : ''
  }
}

// ledgerEntryTitle is the line shown for a ledger entry. The server writes
// expiry and usage descriptions with ids in them ("Expired grant vlx_…"), so
// those read as their plain label; an operator's own description is kept.
export function ledgerEntryTitle(e: { entry_type: string; description?: string }): string {
  if (e.entry_type === 'expiry' || e.entry_type === 'usage') return ledgerEntryLabel(e.entry_type)
  return e.description || ledgerEntryLabel(e.entry_type)
}
