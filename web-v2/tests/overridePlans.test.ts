// The override form lists the prices of the customer's own plans first
// (design §4.13: pick a price of one of the customer's plans). Ended
// subscriptions do not count; a scheduled plan change does.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { customerPlanIds, customerPlansFirst } from '../src/lib/overridePlans.ts'

test('the plans of subscriptions that can still bill, with scheduled moves', () => {
  const ids = customerPlanIds([
    { status: 'active', items: [{ plan_id: 'pln_a' }, { plan_id: 'pln_b', pending_plan_id: 'pln_c' }] },
    { status: 'trialing', items: [{ plan_id: 'pln_d' }] },
    { status: 'draft', items: [{ plan_id: 'pln_e' }] },
    { status: 'canceled', items: [{ plan_id: 'pln_x' }] },
    { status: 'active' },
  ])
  assert.deepEqual([...ids].sort(), ['pln_a', 'pln_b', 'pln_c', 'pln_d', 'pln_e'])
})

test("the customer's plans come first, each group in its own order", () => {
  const prices = [
    { id: 'p1', plan: 'pln_other' },
    { id: 'p2', plan: 'pln_mine' },
    { id: 'p3', plan: 'pln_other' },
    { id: 'p4', plan: 'pln_mine' },
  ]
  const out = customerPlansFirst(prices, p => p.plan, new Set(['pln_mine']))
  assert.deepEqual(out.map(p => p.id), ['p2', 'p4', 'p1', 'p3'])
  assert.deepEqual(customerPlansFirst(prices, p => p.plan, new Set()).map(p => p.id), ['p1', 'p2', 'p3', 'p4'])
})
