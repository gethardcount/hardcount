// How the customer page explains a credit balance.
//
// A $500 grant that was fully applied to an invoice left no trace on the
// customer page: the balance read $0.00, the grants card listed only grants
// with something left, and Outstanding ($4,160) disagreed with the invoice
// total ($4,660) with no word about the $500 of credit between them. These
// helpers give the page the totals, the used-up grants and that sentence.
//
// Dependency-free so node --test can import it.
import { formatCents } from './money.ts'

export interface CreditTotals {
  balance_cents: number
  total_granted: number
  total_used: number
  total_expired: number
}

// creditTotalsLine is "Granted $500.00 · Used $500.00", plus "· Expired $x"
// when some expired. Null when nothing was ever granted.
export function creditTotalsLine(t: CreditTotals, currency: string): string | null {
  if (t.total_granted <= 0) return null
  const parts = [`Granted ${formatCents(t.total_granted, currency)}`, `Used ${formatCents(t.total_used, currency)}`]
  if (t.total_expired > 0) parts.push(`Expired ${formatCents(t.total_expired, currency)}`)
  return parts.join(' · ')
}

export interface GrantRow {
  amount_cents: number
  consumed_cents: number
  remaining_cents: number
  expires_at?: string
}

export type GrantState = 'active' | 'used_up' | 'expired'

// grantState: a grant with nothing left is "used up" when invoices drew all
// of it, "expired" when it ran out of time with some unused.
export function grantState(g: GrantRow, nowMs: number): GrantState {
  if (g.remaining_cents > 0) {
    return g.expires_at && Date.parse(g.expires_at) <= nowMs ? 'expired' : 'active'
  }
  return g.consumed_cents >= g.amount_cents ? 'used_up' : 'expired'
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
