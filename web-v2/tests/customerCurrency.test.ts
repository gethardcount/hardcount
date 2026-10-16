// Pins which currency a customer's money figures use when the figure has no
// currency of its own (credit balance, an empty usage window, a margin with no
// revenue). The defect: a customer on a EUR plan with no billing profile and
// no invoice read $0.00 for its credit balance, usage and margin, because each
// card fell back to the workspace default (USD) instead of the plan.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { customerCurrencyNote, resolveCustomerCurrency } from '../src/lib/customerCurrency.ts'

const plans = [
  { id: 'pln_eur', currency: 'EUR' },
  { id: 'pln_usd', currency: 'USD' },
  { id: 'pln_gbp', currency: 'gbp' },
]

test('a customer on a EUR plan with no profile and no invoice is shown in EUR', () => {
  const c = resolveCustomerCurrency({
    subscriptions: [{ status: 'active', items: [{ plan_id: 'pln_eur' }] }],
    plans,
    billingProfileCurrency: null,
    invoices: [],
    workspaceDefault: 'USD',
  })
  assert.deepEqual(c, { currency: 'EUR', source: 'subscription' })
  assert.equal(customerCurrencyNote(c), 'from the subscription plan')
})

test('the live plan wins over the billing profile and the invoices', () => {
  const c = resolveCustomerCurrency({
    subscriptions: [{ status: 'trialing', items: [{ plan_id: 'pln_eur' }] }],
    plans,
    billingProfileCurrency: 'usd',
    invoices: [{ currency: 'USD', created_at: '2026-10-01T00:00:00Z' }],
    workspaceDefault: 'USD',
  })
  assert.equal(c.currency, 'EUR')
})

test('a canceled subscription does not set the currency', () => {
  const c = resolveCustomerCurrency({
    subscriptions: [{ status: 'canceled', items: [{ plan_id: 'pln_eur' }] }],
    plans,
    billingProfileCurrency: '',
    invoices: [],
    workspaceDefault: 'USD',
  })
  assert.deepEqual(c, { currency: 'USD', source: 'workspace' })
})

test('two live subscriptions: the one whose period started last decides', () => {
  const c = resolveCustomerCurrency({
    subscriptions: [
      { status: 'active', items: [{ plan_id: 'pln_usd' }], current_billing_period_start: '2026-09-01T00:00:00Z' },
      { status: 'active', items: [{ plan_id: 'pln_gbp' }], current_billing_period_start: '2026-10-01T00:00:00Z' },
    ],
    plans,
    workspaceDefault: 'USD',
  })
  assert.deepEqual(c, { currency: 'GBP', source: 'subscription' })
})

test('no live subscription: billing profile, then the newest invoice, then the workspace', () => {
  assert.deepEqual(
    resolveCustomerCurrency({ billingProfileCurrency: 'eur', workspaceDefault: 'USD' }),
    { currency: 'EUR', source: 'billing_profile' },
  )
  assert.deepEqual(
    resolveCustomerCurrency({
      invoices: [
        { currency: 'USD', created_at: '2026-08-01T00:00:00Z' },
        { currency: 'eur', created_at: '2026-10-01T00:00:00Z' },
      ],
      workspaceDefault: 'USD',
    }),
    { currency: 'EUR', source: 'invoice' },
  )
  assert.deepEqual(resolveCustomerCurrency({ workspaceDefault: 'cad' }), { currency: 'CAD', source: 'workspace' })
})

test('a plan the page has not loaded falls through instead of guessing', () => {
  const c = resolveCustomerCurrency({
    subscriptions: [{ status: 'active', items: [{ plan_id: 'pln_unknown' }] }],
    plans,
    billingProfileCurrency: 'eur',
    workspaceDefault: 'USD',
  })
  assert.equal(c.source, 'billing_profile')
})
