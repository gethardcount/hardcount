// The Home page cards: what each one counts and how its figure reads.
// Plain functions so Vitest can check them.

import { amountsByCurrencyText, formatCents, type CurrencyAmount } from './money.ts'

// What each Home card counts, shown under the number on the card.

// creditBalanceByCurrency (internal/analytics/overview.go): the sum of the
// credit ledger, per currency.
export const CREDIT_HELD_DEFINITION =
  'Credit your customers can still spend, in each currency.'

// creditSoldByCurrency (internal/analytics/overview.go): the credit the
// top-ups paid in the period granted, which a discount makes more than the
// payment.
export const CREDIT_SOLD_DEFINITION =
  'Credit bought with top-ups paid in this period. A discounted top-up counts the credit it gave, not its price.'

// sumPaidRevenue (internal/analytics/overview.go): the payments on invoices
// paid in the period. Credit applied is not added again.
export const PAID_DEFINITION =
  'What customers paid for invoices paid in this period, by card or offline. Credit is counted once, when it was bought.'

// usageBilledByCurrency (internal/analytics/overview.go): usage lines on
// invoices issued in the period and not voided.
export const USAGE_BILLED_DEFINITION =
  'Usage on invoices issued in this period. Void invoices are not counted.'

// A card's figure in every currency it has, never one sum across
// currencies. With nothing in any currency it is zero in the workspace
// currency the server returned.
export function amountsCardValue(byCurrency: readonly CurrencyAmount[], currency: string): string {
  return amountsByCurrencyText(byCurrency) ?? formatCents(0, currency)
}

// Revenue and the paid-invoice count cover the workspace default currency
// only (internal/analytics/overview.go): a sum across currencies would mean
// nothing. The hint says so, or a tenant with EUR customers reads a USD
// figure that leaves them out without a word.
export function currencyScopedHint(hint: string, currency: string): string {
  return `${hint} · in ${currency.toUpperCase()} only`
}
