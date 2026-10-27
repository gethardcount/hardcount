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
  const statuses = new Set(['draft', 'trialing', 'active', 'canceled'])
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
  const now = Date.parse('2027-01-15T00:00:00Z')
  assert.deepEqual(collectionPausedFlag({ status: 'active', pause_collection: { behavior: 'keep_as_draft' } }, now), chip)
  assert.deepEqual(collectionPausedFlag({ status: 'trialing', pause_collection: { behavior: 'keep_as_draft' } }, now), chip)
  assert.equal(collectionPausedFlag({ status: 'active' }, now), null)
  // The same live rule as ?collection=paused: a canceled row's leftover
  // pause is not shown.
  assert.equal(collectionPausedFlag({ status: 'canceled', pause_collection: { behavior: 'keep_as_draft' } }, now), null)
})

test('a timed pause stops showing once its resume date passes, by the given now (a test clock)', () => {
  // The engine collects again at resumes_at, though the row can keep the
  // pause until the resumer closes the periods that ended before it.
  const sub = { status: 'active', pause_collection: { behavior: 'keep_as_draft', resumes_at: '2027-01-15T00:00:00Z' } }
  assert.deepEqual(collectionPausedFlag(sub, Date.parse('2027-01-14T00:00:00Z')), { label: 'Collection paused', tone: 'warning' })
  assert.equal(collectionPausedFlag(sub, Date.parse('2027-01-15T00:00:00Z')), null)
  assert.equal(collectionPausedFlag(sub, Date.parse('2027-01-16T00:00:00Z')), null)
})
