// Money formatting. Dependency-free so the unit tests can import it directly;
// lib/api re-exports these for the rest of the dashboard.
//
// The one rule worth repeating: an amount is shown in the currency of the
// record it belongs to (the invoice, the credit note, the customer). There is
// no default: formatCents and getCurrencySymbol require a currency, so the
// type checker lists any call that would guess. A figure the server scoped
// to the workspace default (the Home KPIs) passes the currency the server
// returned with it. An amount whose currency was never recorded (a credit
// row in the audit log) is shown bare, with formatCentsBare.

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

export function getCurrencySymbol(code: string): string {
  return CURRENCY_SYMBOLS[code.toUpperCase()] || code.toUpperCase() + ' '
}

export function formatCents(cents: number, currency: string): string {
  return centsWithSymbol(cents, getCurrencySymbol(currency))
}

// formatCentsBare is an amount with no currency symbol, for a figure whose
// currency was never recorded. Never a guessed symbol.
export function formatCentsBare(cents: number): string {
  return centsWithSymbol(cents, '')
}

function centsWithSymbol(cents: number, symbol: string): string {
  const sign = cents < 0 ? '-' : ''
  const abs = Math.abs(cents)
  return `${sign}${symbol}${Math.floor(abs / 100).toLocaleString('en-US')}.${String(abs % 100).padStart(2, '0')}`
}

// formatCompactCents is a chart-axis amount: "£5.0K" from £1,000 up, whole
// units below. Same currency rule as formatCents. A thousand is 1_000_00
// cents; the Home axis once divided by 100_00 and read $5,000 as "$50.0K".
export function formatCompactCents(cents: number, currency: string): string {
  const sign = cents < 0 ? '-' : ''
  const abs = Math.abs(cents)
  const symbol = getCurrencySymbol(currency)
  if (abs >= 100_000_00) return `${sign}${symbol}${Math.round(abs / 1_000_00)}K`
  if (abs >= 1_000_00) return `${sign}${symbol}${(abs / 1_000_00).toFixed(1)}K`
  return `${sign}${symbol}${Math.round(abs / 100)}`
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

// sumByCurrency adds amounts per currency, never across: one entry per
// currency (codes compared uppercase), zero totals dropped, largest first.
export function sumByCurrency(amounts: readonly CurrencyAmount[]): CurrencyAmount[] {
  const totals = new Map<string, number>()
  for (const a of amounts) {
    const c = a.currency.toUpperCase()
    totals.set(c, (totals.get(c) ?? 0) + a.amount_cents)
  }
  return [...totals]
    .filter(([, cents]) => cents !== 0)
    .map(([currency, amount_cents]) => ({ currency, amount_cents }))
    .sort((a, b) => b.amount_cents - a.amount_cents || a.currency.localeCompare(b.currency))
}

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
// USD runs were active. Nothing at risk is zero in the default currency.
export function atRiskText(stats: {
  at_risk_cents: number
  currency: string
  at_risk_by_currency: readonly CurrencyAmount[]
}): string {
  return amountsByCurrencyText(stats.at_risk_by_currency) ?? formatCents(0, stats.currency)
}

// prorationText is the toast after a subscription item change that prorated.
// The amount is in the subscription's currency (the plans' currency), never
// the workspace default; with the plan not loaded, it has no symbol.
//
// A credit can go two ways at once: to the balance for paid invoices, and off
// the amount due of open ones (#1280). Each part is named with its own amount;
// the whole credit is never called a balance credit.
export function prorationText(
  p: {
    type: 'invoice' | 'credit' | 'adjustment'
    amount_cents: number
    credited_to_balance_cents: number
    reduced_open_invoices_cents: number
  },
  currency: string | undefined,
): string {
  const money = (cents: number) => {
    const abs = Math.abs(cents)
    return currency ? formatCents(abs, currency) : formatCentsBare(abs)
  }
  const amount = money(p.amount_cents)
  if (p.type === 'invoice') return `Proration invoice created for ${amount}`
  if (p.type === 'adjustment') return `Open invoice reduced by ${amount}`
  if (p.credited_to_balance_cents > 0 && p.reduced_open_invoices_cents > 0) {
    return `${money(p.credited_to_balance_cents)} credited to customer balance, and ${money(p.reduced_open_invoices_cents)} taken off open invoices`
  }
  return `${amount} credited to customer balance`
}
