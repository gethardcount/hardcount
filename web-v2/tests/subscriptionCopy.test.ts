// Subscription dialogs and toasts say what really happens, branching on the
// state the server returns.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import {
  cancelImmediatelyText,
  changePlanNowHelper,
  changeQuantityText,
  PAUSE_DIALOG_SENTENCE,
  pauseCollectionToast,
  planChangeToast,
  removeItemText,
  scheduleCancelToast,
} from '../src/lib/subscriptionCopy.ts'
import { cardLabel, removeCardSentence } from '../src/lib/paymentMethodCopy.ts'

test('schedule cancel toast: a trial cancels free at trial end', () => {
  assert.deepEqual(scheduleCancelToast({ status: 'trialing' }, 'Nov 1, 2026'), {
    title: 'Cancellation scheduled',
    description: "Cancels at trial end, Nov 1, 2026. The customer won't be charged.",
  })
  assert.deepEqual(scheduleCancelToast({ status: 'active' }, 'Nov 1, 2026'), {
    title: 'Cancellation scheduled',
    description: 'Cancels at the end of the current period, Nov 1, 2026.',
  })
  assert.equal(scheduleCancelToast({ status: 'trialing' }, undefined).description, 'Cancels at the end of the current period.')
})

test('pause toast names the resume date only when one was saved', () => {
  assert.equal(pauseCollectionToast('Nov 1, 2026').description, 'New invoices are saved as drafts until Nov 1, 2026.')
  assert.equal(pauseCollectionToast(undefined).description, 'New invoices are saved as drafts until you resume collection.')
})

// A draft made while paused stays a draft after the resume
// (subscription.Service.ResumeCollection), so no pause text may say it gets
// charged then.
test('pause words never promise a charge on resume', () => {
  const texts = [PAUSE_DIALOG_SENTENCE, pauseCollectionToast('Nov 1').description!, pauseCollectionToast(undefined).description!]
  for (const t of texts) {
    assert.doesNotMatch(t, /charged until|charged when|charged once|charged after|billed then|skip charge until/i, t)
  }
  assert.match(PAUSE_DIALOG_SENTENCE, /stay drafts after you resume/)
  assert.match(PAUSE_DIALOG_SENTENCE, /finalize them to collect/)
})

// Mid-period item changes: only advance-billed plans settle the difference
// at once; arrears-billed plans bill each part of the period at its end.
test('item change words follow the plan bill timing', () => {
  assert.match(changePlanNowHelper('in_advance'), /charged or credited now/)
  assert.doesNotMatch(changePlanNowHelper('in_arrears'), /now\.$|credited/)
  assert.match(changePlanNowHelper('in_arrears'), /billed at its end/)
  assert.equal(changePlanNowHelper(undefined), 'The new price applies from now.')

  assert.match(changeQuantityText('in_advance'), /a decrease is credited/)
  assert.doesNotMatch(changeQuantityText('in_arrears'), /credited|charged for the rest/)
  assert.match(changeQuantityText('in_arrears'), /billed at its end/)
  assert.equal(changeQuantityText(undefined), 'Takes effect now.')

  assert.match(removeItemText('in_advance'), /unused part of the period is credited/)
  assert.doesNotMatch(removeItemText('in_arrears'), /credited/)
  assert.match(removeItemText('in_arrears'), /billed at the period end/)
  assert.equal(removeItemText(undefined), '')
})

test('cancel immediately: a final invoice for a paid subscription, nothing for a trial', () => {
  assert.match(cancelImmediatelyText('active'), /final invoice/)
  assert.doesNotMatch(cancelImmediatelyText('active'), /Stops billing right now/)
  assert.match(cancelImmediatelyText('trialing'), /Nothing is billed/)
  assert.doesNotMatch(cancelImmediatelyText('trialing'), /final invoice/)
})

test('plan change toast: scheduled vs changed now', () => {
  assert.deepEqual(planChangeToast({ pending_plan_id: 'p2' }, 'Nov 1, 2026'), {
    title: 'Plan change scheduled',
    description: 'The new plan starts Nov 1, 2026.',
  })
  assert.equal(planChangeToast({ pending_plan_id: 'p2' }, undefined).description, 'The new plan starts at the next billing period.')
  assert.deepEqual(planChangeToast({ pending_plan_id: null }, 'ignored'), { title: 'Plan changed' })
})

test('remove card: only card, default with others left, non-default', () => {
  assert.equal(cardLabel({ card_brand: 'visa', card_last4: '4242' }), 'Visa ····4242')
  assert.equal(cardLabel({}), 'Card ····????')
  assert.match(removeCardSentence({ onlyCard: true, isDefault: true }), /only card/)
  assert.doesNotMatch(removeCardSentence({ onlyCard: true, isDefault: true }), /emailed/)
  assert.match(removeCardSentence({ onlyCard: false, isDefault: true }), /newest remaining card becomes the default/)
  assert.match(removeCardSentence({ onlyCard: false, isDefault: false }), /default card stays the same/)
})
