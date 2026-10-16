// Money formatting. Dependency-free so the unit tests can import it directly;
// lib/api re-exports these for the rest of the dashboard.
//
// The one rule worth repeating: an amount that belongs to an invoice is shown
// in THAT invoice's currency. formatCents(cents) with no currency falls back to
// the workspace default, which is right only for figures the server already
// scoped to the default currency (dashboard KPIs, credit balances).

const CURRENCY_SYMBOLS: Record<string, string> = {
  USD: '$', EUR: '€', GBP: '£', JPY: '¥', CNY: '¥',
  INR: '₹', BRL: 'R$', CAD: 'CA$', AUD: 'A$', CHF: 'CHF ',
  SGD: 'S$', HKD: 'HK$', NZD: 'NZ$', SEK: 'kr ', NOK: 'kr ',
  DKK: 'kr ', ZAR: 'R', KRW: '₩', MXN: 'MX$', PLN: 'zł',
  CZK: 'Kč ', HUF: 'Ft ', ILS: '₪', AED: 'AED ', SAR: 'SAR ',
  THB: '฿', MYR: 'RM ', IDR: 'Rp ', PHP: '₱', VND: '₫',
  CLP: 'CL$', COP: 'CO$', PEN: 'S/', ARS: 'AR$', TWD: 'NT$',
}

let _activeCurrency = 'USD'

export function setActiveCurrency(code: string) {
  _activeCurrency = code.toUpperCase()
}

export function getActiveCurrency(): string {
  return _activeCurrency
}

export function getCurrencySymbol(code?: string): string {
  return CURRENCY_SYMBOLS[(code || _activeCurrency).toUpperCase()] || (code || _activeCurrency) + ' '
}

export function formatCents(cents: number, currency?: string): string {
  const sign = cents < 0 ? '-' : ''
  const abs = Math.abs(cents)
  const symbol = getCurrencySymbol(currency)
  return `${sign}${symbol}${Math.floor(abs / 100).toLocaleString('en-US')}.${String(abs % 100).padStart(2, '0')}`
}

// amountInputToCents turns what an operator typed in an amount field into the
// cents a form submits. A confirm step that restates the amount must use this
// same conversion, so it never shows a figure different from the one sent.
export function amountInputToCents(input: string): number {
  const n = parseFloat(input || '0')
  if (!Number.isFinite(n)) return 0
  return Math.round(n * 100)
}

export interface CurrencyAmount { currency: string; amount_cents: number }

// amountsByCurrencyText renders an aggregate over invoices in several
// currencies: one figure per currency, joined with "+", never one sum that adds
// cents of different currencies. Zero entries are dropped; beyond three
// currencies the tail is counted instead of listed, to keep it one line.
// Returns null when nothing is left to show.
export function amountsByCurrencyText(byCurrency: readonly CurrencyAmount[]): string | null {
  const owed = byCurrency.filter(a => a.amount_cents > 0)
  if (owed.length === 0) return null
  const shown = owed.length > 3 ? owed.slice(0, 2) : owed
  const parts = shown.map(a => formatCents(a.amount_cents, a.currency))
  const rest = owed.length - shown.length
  if (rest > 0) parts.push(`${rest} other currencies`)
  return parts.join(' + ')
}

// outstandingText renders the Dashboard's "N open invoices · X outstanding"
// row. The count covers every currency, so the amount must too.
export function outstandingText(
  openInvoices: number,
  byCurrency: readonly CurrencyAmount[],
): string {
  const count = `${openInvoices} open ${openInvoices === 1 ? 'invoice' : 'invoices'}`
  const amounts = amountsByCurrencyText(byCurrency)
  return amounts ? `${count} · ${amounts} outstanding` : count
}

// atRiskText is the Dunning "At risk" card's figure. at_risk_by_currency names
// every currency with active or escalated runs; at_risk_cents alone covers only
// the workspace default, so after the default changed it read CA$0.00 while
// USD runs were active. The fallback serves a server that predates the list.
export function atRiskText(stats: {
  at_risk_cents: number
  currency?: string
  at_risk_by_currency?: readonly CurrencyAmount[]
}): string {
  if (stats.at_risk_by_currency) {
    return amountsByCurrencyText(stats.at_risk_by_currency) ?? formatCents(0, stats.currency)
  }
  return formatCents(stats.at_risk_cents, stats.currency)
}

// prorationText is the toast after a subscription item change that prorated.
// The amount is in the subscription's currency (the plans' currency), never
// the workspace default.
export function prorationText(
  p: { type: 'invoice' | 'credit' | 'adjustment'; amount_cents: number },
  currency: string | undefined,
): string {
  const amount = formatCents(Math.abs(p.amount_cents), currency)
  if (p.type === 'invoice') return `Proration invoice created for ${amount}`
  if (p.type === 'adjustment') return `Open invoice reduced by ${amount}`
  return `${amount} credited to customer balance`
}

// dunningAmountDue is the amount a dunning run is chasing, in its invoice's
// currency. The run carries both from a server-side join; a run whose join came
// back empty falls back to the invoice loaded on the page (the same invoice).
export function dunningAmountDue(
  run: { invoice_amount_due_cents?: number; invoice_currency?: string },
  inv?: { amount_due_cents: number; currency: string },
): string | null {
  if (run.invoice_amount_due_cents !== undefined && run.invoice_amount_due_cents !== 0) {
    return formatCents(run.invoice_amount_due_cents, run.invoice_currency || inv?.currency)
  }
  return inv ? formatCents(inv.amount_due_cents, inv.currency) : null
}
