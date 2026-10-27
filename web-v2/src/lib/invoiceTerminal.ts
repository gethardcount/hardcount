/**
 * Which invoice states have stopped being collectible, and what to call the
 * money once they have.
 *
 * This exists because the same rule has now been missed three times, always
 * the same way — a branch tests `voided` (with or without `paid`) and forgets
 * `uncollectible`, so a written-off invoice keeps demanding payment:
 *
 *   1. #651 relabelled the figure on the hosted invoice's totals block and
 *      left its twin in the header saying "Amount due" — that page's own
 *      comment records the fix as "Same rule, both places."
 *   2. The public payment-update page never read invoice status at all: a
 *      VOIDED invoice rendered "Payment method needed · Amount due $25.00"
 *      (found on the FLOW D4 walk, 2026-07-28).
 *   3. That fix covered `paid || voided` and stopped there, so a WRITTEN-OFF
 *      invoice still showed the amber demand — while the hosted invoice page,
 *      facing the same customer from the same dunning email, already said
 *      "This invoice is closed". Two public pages, opposite answers, same
 *      invoice (found live on TC Walk Co VLX-000152, 2026-08-05).
 *
 * Three misses of one rule is the point at which a shared predicate is cheaper
 * than the next miss. Both public pages now read this, and it is pinned by
 * tests/invoiceTerminal.test.ts.
 *
 * NOTE the copy is deliberately NOT shared. The hosted invoice page drops its
 * Pay button and says "contact support"; the payment-update page keeps its
 * "add a payment method" button, because saving a card still serves the NEXT
 * invoice even when this one is closed. Same predicate, different voice.
 */

import { formatCents } from './money.ts'
import { paymentIsUnresolved } from './status.ts'

/**
 * Terminal = collection has ended, by any route. `uncollectible` belongs here
 * even though a write-off does NOT annul the debt (the invoice stays on the
 * books and MarkPaid remains legal from it) — what ended is SELF-SERVICE
 * collection, which is the only thing these customer-facing pages offer.
 */
export const TERMINAL_INVOICE_STATUSES = ['paid', 'voided', 'uncollectible'] as const

export function isTerminalInvoiceStatus(status: string | undefined | null): boolean {
  return !!status && (TERMINAL_INVOICE_STATUSES as readonly string[]).includes(status)
}

/**
 * The label for `amount_due_cents`, which means different things by status:
 *
 * - void / write-off: it SURVIVES — the transition ends collection, it does not
 *   rewrite the figure — so it is the invoice's amount, and calling it "Amount
 *   Due" on an invoice nobody is collecting is the lie.
 * - paid: it is ZEROED — the settle moves it into `amount_paid_cents`
 *   (MarkPaid). Labelled "Invoice amount" it read "Invoice amount $0.00" on a
 *   paid invoice (found walking FLOW I10, 2026-10-11); "Amount due $0.00" is
 *   the true statement. Pages that headline a paid invoice show
 *   `amount_paid_cents` instead.
 */
export function invoiceAmountLabel(status: string | undefined | null): string {
  if (status === 'paid') return 'Amount due'
  return isTerminalInvoiceStatus(status) ? 'Invoice amount' : 'Amount due'
}

// ── Every screen's reads of an invoice's money and payment fields ──────────
//
// `payment_status` and `amount_due_cents` mean different things by status: a
// void keeps both as they were, a settle zeroes amount_due, a write-off is no
// longer collected. Read raw, a voided invoice showed "pending" and "$29.00
// due" on the subscription page, Home, the command palette, the Invoices and
// Dunning lists and the customer's "Last invoice", while its own page said
// $0.00 (product sweep 2026-10-21, A7 / E1 / E2 / E3). Screens read these
// fields only through the helpers below; the lint rule in eslint.config.js
// fails a raw read anywhere outside src/lib.

export interface InvoiceMoneyInput {
  status?: string | null
  payment_status?: string
  amount_due_cents: number
  amount_paid_cents?: number
  credits_applied_cents?: number
  currency: string
}

/**
 * What is still owed, as the invoice's own page states it. A void annuls the
 * debt, so nothing is owed even though amount_due survives. A write-off stays
 * on the books (ADR-110: MarkPaid remains legal), so it still owes amount_due.
 * A paid invoice's amount_due is already zero.
 */
export function invoiceDueCents(inv: InvoiceMoneyInput): number {
  return inv.status === 'voided' ? 0 : inv.amount_due_cents
}

/**
 * The invoice's amount on a row: what a list shows under "Amount", and what
 * the server's amount sort orders by (invoiceSortColumn in
 * internal/invoice/postgres.go). Paid: what the payment and the customer
 * credit applied paid together, because the settle zeroed amount_due and
 * credit never reaches amount_paid: an invoice a prepaid commit paid in full
 * read "$0.00 paid". Everything else: amount_due, which a void and a write-off
 * keep, so a void reads the amount it was voided at. Not what is owed: that
 * is invoiceDueCents, and a void owes nothing.
 */
export function invoiceAmountCents(inv: InvoiceMoneyInput): number {
  if (inv.status === 'paid') return (inv.amount_paid_cents ?? 0) + (inv.credits_applied_cents ?? 0)
  return inv.amount_due_cents
}

/**
 * The amount a row shows beside the invoice's status badge, with the word that
 * says what it is. Open: "$29.00 due". Paid: "$29.00 paid". Draft, void and
 * write-off: the invoice amount, bare; the badge beside it says which.
 */
export function invoiceRowAmount(inv: InvoiceMoneyInput): string {
  const amount = formatCents(invoiceAmountCents(inv), inv.currency)
  if (inv.status === 'paid') return `${amount} paid`
  return inv.status === 'finalized' ? `${amount} due` : amount
}

/** The public pages' amount line: amount_due under the status-aware label. */
export function invoiceAmountLine(inv: InvoiceMoneyInput): { label: string; cents: number } {
  return { label: invoiceAmountLabel(inv.status), cents: inv.amount_due_cents }
}

/** The headline figure: a paid invoice leads with what was paid. */
export function invoiceHeadline(inv: InvoiceMoneyInput): { label: string; cents: number } {
  if (inv.status === 'paid') return { label: 'Amount paid', cents: inv.amount_paid_cents ?? 0 }
  return invoiceAmountLine(inv)
}

export function invoicePaymentSucceeded(inv: InvoiceMoneyInput): boolean {
  return inv.payment_status === 'succeeded'
}

/**
 * Which unresolved payment the invoice carries: 'processing' (a charge still
 * confirming) or 'unknown' (parked: Stripe's answer was unclear, ADR-107), or
 * null. Same rule as paymentIsUnresolved in lib/status.
 */
export function invoicePaymentInFlight(inv: InvoiceMoneyInput): 'processing' | 'unknown' | null {
  if (!inv.payment_status || !paymentIsUnresolved(inv.payment_status)) return null
  return inv.payment_status === 'processing' ? 'processing' : 'unknown'
}

/** Open, something due, and no payment succeeded or unresolved: a charge can start. */
export function invoiceCanCollect(inv: InvoiceMoneyInput): boolean {
  return inv.status === 'finalized' &&
    !invoicePaymentSucceeded(inv) &&
    invoicePaymentInFlight(inv) === null &&
    inv.amount_due_cents > 0
}

/**
 * Whether the invoice page offers Collect payment. A charge needs a card, so
 * with none it is hidden; except on an invoice Stripe refused as below its
 * minimum charge. Collect applies the customer's credit balance there first
 * and pays the invoice when that covers it, which needs no card (with a card
 * it then asks Stripe again).
 */
export function invoiceShowsCollect(inv: InvoiceMoneyInput, hasPaymentMethod: boolean, attentionReason?: string): boolean {
  return invoiceCanCollect(inv) && (hasPaymentMethod || attentionReason === 'amount_below_minimum')
}

/**
 * Open with no payment succeeded or processing. A parked charge ('unknown')
 * still awaits payment: mark uncollectible is its way out (ADR-107).
 */
export function invoiceAwaitingPayment(inv: InvoiceMoneyInput): boolean {
  return inv.status === 'finalized' &&
    !invoicePaymentSucceeded(inv) &&
    inv.payment_status !== 'processing'
}

type DunningRunInvoiceJoin = { invoice_amount_due_cents?: number; invoice_currency?: string }

// The server-side join, for a run whose invoice did not load on the page. It
// leaves the currency empty when it found no invoice.
function dunningJoinAmount(run: DunningRunInvoiceJoin): string | null {
  if (run.invoice_currency && run.invoice_amount_due_cents !== undefined) {
    return formatCents(run.invoice_amount_due_cents, run.invoice_currency)
  }
  return null
}

/**
 * The amount a dunning run is chasing, in its invoice's currency: the resolve
 * dialog's "Amount due". The invoice loaded on the page decides, because it
 * carries the status: a run on a voided invoice is chasing nothing, though the
 * joined amount_due survived the void (sweep A7 / E1).
 */
export function dunningAmountDue(run: DunningRunInvoiceJoin, inv?: InvoiceMoneyInput): string | null {
  if (inv) return formatCents(invoiceDueCents(inv), inv.currency)
  return dunningJoinAmount(run)
}

/**
 * The Dunning list's "Amount": the run's invoice amount (invoiceAmountCents),
 * so a run closed by a void reads the amount it was voided at. Showing what
 * was owed instead turned the whole Resolved tab into $0.00 rows (sign-off of
 * #1328).
 */
export function dunningRunAmount(run: DunningRunInvoiceJoin, inv?: InvoiceMoneyInput): string | null {
  if (inv) return formatCents(invoiceAmountCents(inv), inv.currency)
  return dunningJoinAmount(run)
}
