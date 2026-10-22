// The Subscriptions list tabs and the API filter each one sends.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { SUBSCRIPTION_TABS, subscriptionTabEmptyTitle, subscriptionTabParams } from '../src/lib/subscriptionListFilters.ts'
import { collectionPausedFlag } from '../src/lib/statusLabels.ts'

test('the Paused tab asks for paused collection, not a status no subscription has', () => {
  // A paused subscription stays active; only its collection pauses. The tab
  // used to send ?status=paused and was always empty (sweep E6).
  assert.deepEqual(subscriptionTabParams('paused'), { collection: 'paused' })
  assert.deepEqual(subscriptionTabParams('billing_failing'), { billing: 'failing' })
  assert.deepEqual(subscriptionTabParams('active'), { status: 'active' })
  assert.deepEqual(subscriptionTabParams(''), {})
})

test('every tab maps to a filter the API accepts', () => {
  const statuses = new Set(['draft', 'trialing', 'active', 'canceled', 'archived'])
  for (const tab of SUBSCRIPTION_TABS) {
    const params = subscriptionTabParams(tab.value)
    if (params.status !== undefined) assert.ok(statuses.has(params.status), `${tab.value} sends status=${params.status}`)
    if (params.collection !== undefined) assert.equal(params.collection, 'paused')
    if (params.billing !== undefined) assert.equal(params.billing, 'failing')
  }
  assert.ok(SUBSCRIPTION_TABS.some(t => t.value === 'paused' && t.label === 'Collection paused'))
})

test('an empty tab says what it filtered for', () => {
  assert.equal(subscriptionTabEmptyTitle('paused'), 'No subscriptions with collection paused')
  assert.equal(subscriptionTabEmptyTitle('billing_failing'), 'No subscriptions with failing billing')
  assert.equal(subscriptionTabEmptyTitle('canceled'), 'No canceled subscriptions')
})

test('the pause chip shows on live subscriptions whose collection is paused', () => {
  const chip = { label: 'Collection paused', tone: 'warning' }
  assert.deepEqual(collectionPausedFlag({ status: 'active', pause_collection: { behavior: 'keep_as_draft' } }), chip)
  assert.deepEqual(collectionPausedFlag({ status: 'trialing', pause_collection: { behavior: 'keep_as_draft' } }), chip)
  assert.equal(collectionPausedFlag({ status: 'active' }), null)
  // The same live rule as ?collection=paused: a canceled row's leftover
  // pause is not shown.
  assert.equal(collectionPausedFlag({ status: 'canceled', pause_collection: { behavior: 'keep_as_draft' } }), null)
})
