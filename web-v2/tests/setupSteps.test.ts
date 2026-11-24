// The setup list follows the operator's job in order, and each step is done
// from one fact the hook reads.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { hasPricedPlan, isGatewayKey, setupProgress, setupSteps, type SetupFacts } from '../src/lib/setupSteps.ts'

const NONE: SetupFacts = {
  stripe: 'none',
  gatewayKey: false,
  pricedPlan: false,
  providerCost: false,
  subscription: false,
  usage: false,
  credit: false,
}

test('the steps follow the job, in order', () => {
  assert.deepEqual(setupSteps(NONE).map(s => s.label), [
    'Connect Stripe',
    'Make a key for your gateway',
    'Set your prices',
    'Record what providers charge you',
    'Add a customer and put them on a plan',
    'Send usage from your gateway',
    'Sell a customer credit',
  ])
  assert.ok(setupSteps(NONE).every(s => !s.done))
})

test('each step is done from its own fact', () => {
  const facts: [keyof SetupFacts, SetupFacts[keyof SetupFacts], string][] = [
    ['stripe', 'connected', 'stripe'],
    ['gatewayKey', true, 'key'],
    ['pricedPlan', true, 'prices'],
    ['providerCost', true, 'costs'],
    ['subscription', true, 'subscription'],
    ['usage', true, 'usage'],
    ['credit', true, 'credit'],
  ]
  for (const [fact, value, key] of facts) {
    const done = setupSteps({ ...NONE, [fact]: value }).filter(s => s.done).map(s => s.key)
    assert.deepEqual(done, [key], `${fact} ticks ${key} and nothing else`)
  }
})

test('a key Stripe refused, or no answer yet, does not connect Stripe', () => {
  const refused = setupSteps({ ...NONE, stripe: 'refused' })[0]
  assert.equal(refused.done, false)
  assert.equal(refused.desc, 'Stripe refused the saved key. Paste the current one from your Stripe dashboard.')
  assert.deepEqual(refused.actions, [{ label: 'Replace key', to: '/settings?tab=payments' }])
  assert.equal(setupSteps({ ...NONE, stripe: undefined })[0].done, false)
})

test('prices offer a recipe or your own, each on its Pricing tab', () => {
  const prices = setupSteps(NONE).find(s => s.key === 'prices')!
  assert.deepEqual(prices.actions, [
    { label: 'Start from a recipe', to: '/pricing?tab=recipes' },
    { label: 'Set them yourself', to: '/pricing' },
  ])
  const costs = setupSteps(NONE).find(s => s.key === 'costs')!
  assert.deepEqual(costs.actions.map(a => a.to), ['/pricing?tab=provider-costs'])
  assert.match(costs.desc, /margin/)
  assert.match(costs.desc, /before usage arrives/)
  // Sell credit is an action on the Credit page.
  assert.deepEqual(setupSteps(NONE).find(s => s.key === 'credit')!.actions.map(a => a.to), ['/credits'])
})

test('provider costs are optional: not counted, and the list finishes without them', () => {
  assert.deepEqual(setupSteps(NONE).filter(s => s.optional).map(s => s.key), ['costs'])
  assert.deepEqual(setupProgress(setupSteps(NONE)), { complete: 0, total: 6 })
  assert.deepEqual(setupProgress(setupSteps({ ...NONE, providerCost: true })), { complete: 0, total: 6 })
  const allButCosts: SetupFacts = { ...NONE, stripe: 'connected', gatewayKey: true, pricedPlan: true, subscription: true, usage: true, credit: true }
  assert.deepEqual(setupProgress(setupSteps(allButCosts)), { complete: 6, total: 6 })
})

test('a gateway key is an active usage-only key; the secret keys an account starts with are not', () => {
  assert.equal(isGatewayKey({ key_type: 'ingest', status: 'active' }), true)
  assert.equal(isGatewayKey({ key_type: 'secret', status: 'active' }), false)
  assert.equal(isGatewayKey({ key_type: 'ingest', status: 'revoked' }), false)
  assert.equal(isGatewayKey({ key_type: 'ingest', status: 'expired' }), false)
})

test('prices are set once a plan has a usage price; a base fee alone is not one', () => {
  assert.equal(hasPricedPlan([]), false)
  assert.equal(hasPricedPlan([{ prices: [] }, { prices: null }, {}]), false)
  assert.equal(hasPricedPlan([{ prices: [] }, { prices: [{ id: 'hc_prc_1' }] }]), true)
})
