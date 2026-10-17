// New accounts start with a seeded default policy (ADR-123); older accounts
// may have none. The pages must say which, and a recipe preview must say
// whether its policy will take over the default.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { newPolicyDefaultNote, recipeRetryNote, retriesOffNotice } from '../src/lib/dunningCoverage.ts'

const seed = { name: 'Default retries', enabled: true, is_default: true, seeded: true }
const mine = { name: 'Net 30', enabled: true, is_default: true, seeded: false }

test('no policy at all: failed payments are not retried', () => {
  const n = retriesOffNotice([])
  assert.equal(n?.title, 'Failed payments are not retried')
  assert.equal(n?.body, 'Create a policy to start automatic retries.')
})

test('a disabled default is called out by name', () => {
  const n = retriesOffNotice([{ ...mine, enabled: false }])
  assert.equal(n?.title, 'Default policy is turned off')
  assert.match(n?.body ?? '', /Net 30/)
})

test('an enabled default needs no notice, seeded or not', () => {
  assert.equal(retriesOffNotice([seed]), null)
  assert.equal(retriesOffNotice([mine, { ...seed, is_default: false }]), null)
})

test('recipe preview: what happens to the default', () => {
  assert.equal(recipeRetryNote([]), 'becomes your default')
  assert.equal(recipeRetryNote([seed]), 'becomes your default, replacing Default retries')
  assert.equal(recipeRetryNote([mine]), 'added next to your default, Net 30')
})

test('create dialog: says when saving replaces the seeded default', () => {
  assert.equal(
    newPolicyDefaultNote([seed], true),
    'Saving makes this your default: every customer without their own policy uses it instead of Default retries.',
  )
  // A policy created turned off does not take over (ADR-123).
  assert.equal(newPolicyDefaultNote([seed], false), 'Default retries stays your default while this policy is turned off.')
  // Only the seeded default hands over; an operator's own default stays.
  assert.equal(newPolicyDefaultNote([mine], true), null)
  assert.equal(newPolicyDefaultNote([mine, { ...seed, is_default: false }], true), null)
})
