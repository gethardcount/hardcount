// Pins how the customer page reads a billing-profile fetch.
//
// The defect this exists for: every failed fetch was read as "this customer
// has no billing profile", so a 500 or a dropped connection drew the "Set Up
// Billing Profile" button over a profile that exists. Saving that blank form
// replaces the whole profile, which resets the tax status and clears the tax
// ID, and that changes the tax on later invoices.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { profileOrAbsent, billingProfileCard } from '../src/lib/billingProfileLoad.ts'

const is404 = (e: unknown) => (e as { status?: number })?.status === 404
const profile = { customer_id: 'vlx_cus_1', tax_status: 'exempt' }

test('a 404 means the customer has no profile yet', async () => {
  assert.equal(await profileOrAbsent(async () => { throw { status: 404 } }, is404), null)
})

test('a 500 on the profile GET is a failure, not a missing profile', async () => {
  await assert.rejects(profileOrAbsent(async () => { throw { status: 500 } }, is404))
  await assert.rejects(profileOrAbsent(async () => { throw new TypeError('network') }, is404))
})

test('a loaded profile passes through', async () => {
  assert.equal(await profileOrAbsent(async () => profile, is404), profile)
})

// The card draws the Set Up button only for 'absent' and the Edit button only
// for 'profile', so these states are what keep the buttons off the page.
test('a failed fetch does not offer Set Up or Edit', () => {
  assert.equal(billingProfileCard({ data: undefined, isError: true }), 'error')
})

test('a fetch still in flight does not offer Set Up', () => {
  assert.equal(billingProfileCard({ data: undefined, isError: false }), 'loading')
})

test('a failed refetch hides the buttons even with an earlier answer on screen', () => {
  // After a save, the page refetches. If that refetch fails, the earlier
  // answer is out of date: "no profile" may now be wrong, and an Edit form
  // filled from the old values would undo the save.
  assert.equal(billingProfileCard({ data: null, isError: true }), 'error')
  assert.equal(billingProfileCard({ data: profile, isError: true }), 'error')
})

test('Set Up is offered only once the server has said there is no profile', () => {
  assert.equal(billingProfileCard({ data: null, isError: false }), 'absent')
})

test('Edit is offered for a loaded profile', () => {
  assert.equal(billingProfileCard({ data: profile, isError: false }), 'profile')
})
