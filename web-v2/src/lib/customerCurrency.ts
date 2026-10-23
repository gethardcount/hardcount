// The one answer to "which currency is this customer billed in?".
//
// Every money figure on a customer that has no currency of its own (an empty
// usage window, a margin with no revenue yet, the default of the New invoice
// and Grant credits dialogs) is shown in this currency. Before, each
// card fell back on its own: the credit balance took the billing profile and
// then the invoices, the usage and margin cards took the workspace default. A
// customer on a EUR plan with no profile and no invoice yet read $0.00 in
// three places and €45.00 in the fourth.
//
// Order, first match wins. It is the order the billing engine draws an
// invoice in (engine.invoiceCurrency, ADR-100), so the page shows the
// currency the next invoice will use:
//  1. the billing profile's currency;
//  2. the plan of the customer's draft, trialing or active subscription (the
//     one whose period started last, the same "primary" pick
//     GET /v1/customers/{id}/usage makes). A plan has exactly one currency;
//  3. the newest invoice's currency;
//  4. the workspace default.
//
// Credit is not such a figure: each ledger row stores its own currency and
// pays only invoices in it (ADR-129), so credit amounts are shown in theirs.

export type CustomerCurrencySource = 'subscription' | 'billing_profile' | 'invoice' | 'workspace'

export interface CustomerCurrency {
  currency: string
  source: CustomerCurrencySource
}

// A subscription in these states fixes the customer's currency. They are the
// states POST /v1/invoices pins a manual invoice to (ADR-100): a draft counts,
// because it will bill in its plan's currency once it starts.
const LIVE_STATUSES = new Set(['draft', 'trialing', 'active'])

export interface CurrencySubscription {
  status: string
  items?: readonly { plan_id: string }[]
  current_billing_period_start?: string
  created_at?: string
}

export interface CurrencyInput {
  subscriptions?: readonly CurrencySubscription[]
  plans?: readonly { id: string; currency: string }[]
  billingProfileCurrency?: string | null
  invoices?: readonly { currency?: string; created_at?: string }[]
  workspaceDefault: string
}

export function resolveCustomerCurrency(input: CurrencyInput): CustomerCurrency {
  if (input.billingProfileCurrency) {
    return { currency: input.billingProfileCurrency.toUpperCase(), source: 'billing_profile' }
  }
  const planCurrency = new Map((input.plans ?? []).map(p => [p.id, p.currency]))
  const live = (input.subscriptions ?? [])
    .filter(s => LIVE_STATUSES.has(s.status))
    .sort((a, b) =>
      (b.current_billing_period_start ?? b.created_at ?? '').localeCompare(a.current_billing_period_start ?? a.created_at ?? ''))
  for (const sub of live) {
    for (const item of sub.items ?? []) {
      const c = planCurrency.get(item.plan_id)
      if (c) return { currency: c.toUpperCase(), source: 'subscription' }
    }
  }
  const newest = [...(input.invoices ?? [])]
    .filter(i => i.currency)
    .sort((a, b) => (b.created_at ?? '').localeCompare(a.created_at ?? ''))[0]
  if (newest?.currency) return { currency: newest.currency.toUpperCase(), source: 'invoice' }
  return { currency: input.workspaceDefault.toUpperCase(), source: 'workspace' }
}

// The state of one query feeding the answer (a react-query result fits).
export interface CurrencyQueryState {
  isLoading: boolean
  isError: boolean
}

// settledCustomerCurrency answers only once every query has loaded without
// error. While one loads, answering with the workspace default showed a EUR
// customer's figures in dollars for a moment. When one has failed, the
// fallback would be a guess: a failed billing-profile read would skip the
// profile's currency, and a New invoice dialog would open in a currency the
// engine then refuses. Undefined makes the page show "—" and keep New invoice
// disabled.
export function settledCustomerCurrency(
  queries: readonly CurrencyQueryState[],
  input: () => CurrencyInput,
): CustomerCurrency | undefined {
  if (queries.some(q => q.isLoading || q.isError)) return undefined
  return resolveCustomerCurrency(input())
}

// invoiceCurrencyPin is why a manual invoice can only be in the customer's
// currency, or null when it can be in any. POST /v1/invoices refuses another
// currency (409 customer_currency_conflict) when the billing profile sets it
// or a live subscription's plan does; the newest invoice and the workspace
// default only suggest one. The New invoice dialog fixes the picker and
// shows this line.
export function invoiceCurrencyPin(c: CustomerCurrency): string | null {
  switch (c.source) {
    case 'billing_profile':
      return `This customer is billed in ${c.currency} (billing profile), so the invoice is too.`
    case 'subscription':
      return `This customer is billed in ${c.currency} (subscription plan), so the invoice is too.`
    default:
      return null
  }
}
