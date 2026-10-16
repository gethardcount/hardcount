// The one answer to "which currency is this customer billed in?".
//
// Every money figure on a customer that has no currency of its own (the
// credit balance, an empty usage window, a margin with no revenue yet, the
// default of the New invoice dialog) is shown in this currency. Before, each
// card fell back on its own: the credit balance took the billing profile and
// then the invoices, the usage and margin cards took the workspace default. A
// customer on a EUR plan with no profile and no invoice yet read $0.00 in
// three places and €45.00 in the fourth.
//
// Order, first match wins:
//  1. the plan of the customer's live subscription (the one whose period
//     started last, the same "primary" pick GET /v1/customers/{id}/usage
//     makes). A plan has exactly one currency and it is what the next invoice
//     is drawn in;
//  2. the billing profile's currency override;
//  3. the newest invoice's currency;
//  4. the workspace default.
//
// Credits have no currency of their own: the ledger is drawn 1:1 against
// whatever invoice they are applied to (ADR-100), which is why the credit
// balance follows this answer too.
//
// Dependency-free so node --test can import it.

export type CustomerCurrencySource = 'subscription' | 'billing_profile' | 'invoice' | 'workspace'

export interface CustomerCurrency {
  currency: string
  source: CustomerCurrencySource
}

// A subscription still bills (and so still sets the currency) in these states.
const LIVE_STATUSES = new Set(['active', 'trialing', 'past_due', 'paused'])

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
  if (input.billingProfileCurrency) {
    return { currency: input.billingProfileCurrency.toUpperCase(), source: 'billing_profile' }
  }
  const newest = [...(input.invoices ?? [])]
    .filter(i => i.currency)
    .sort((a, b) => (b.created_at ?? '').localeCompare(a.created_at ?? ''))[0]
  if (newest?.currency) return { currency: newest.currency.toUpperCase(), source: 'invoice' }
  return { currency: input.workspaceDefault.toUpperCase(), source: 'workspace' }
}

// customerCurrencyNote is the short provenance line shown under the currency
// in the customer's details, so an operator can see why a figure is in €.
export function customerCurrencyNote(c: CustomerCurrency): string {
  switch (c.source) {
    case 'subscription': return 'from the subscription plan'
    case 'billing_profile': return 'set on the billing profile'
    case 'invoice': return 'from the latest invoice'
    case 'workspace': return 'workspace default'
  }
}
