// Money formatting. Dependency-free so node --test can import it directly;
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

// outstandingText renders the Dashboard's "N open invoices · X outstanding"
// row. The count covers every currency, so the amount must too: one figure
// per currency, joined with "+", never a default-currency sum beside a count
// that includes other currencies. Beyond three currencies the tail is counted
// instead of listed, to keep the row one line.
export function outstandingText(
  openInvoices: number,
  byCurrency: readonly { currency: string; amount_cents: number }[],
): string {
  const owed = byCurrency.filter(a => a.amount_cents > 0)
  const count = `${openInvoices} open ${openInvoices === 1 ? 'invoice' : 'invoices'}`
  if (owed.length === 0) return count
  const shown = owed.length > 3 ? owed.slice(0, 2) : owed
  const parts = shown.map(a => formatCents(a.amount_cents, a.currency))
  const rest = owed.length - shown.length
  if (rest > 0) parts.push(`${rest} other currencies`)
  return `${count} · ${parts.join(' + ')} outstanding`
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
