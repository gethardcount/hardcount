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

// A pause does not stop a dunning run already open on an unpaid invoice
// (ADR-036 amendment 2026-10-22; Stripe keeps retrying invoices created
// before a pause too). The dialog is where the operator decides, so it says
// the retries continue and how to stop them.
test('pause dialog says retries already running continue', () => {
  assert.match(PAUSE_DIALOG_SENTENCE, /retries already running on unpaid invoices continue/i)
  assert.match(PAUSE_DIALOG_SENTENCE, /To stop them, void each invoice or mark it uncollectible\./)
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
  // #1280: a swap to another interval says which invoice it created and for how much.
  assert.deepEqual(planChangeToast({ pending_plan_id: null }, undefined, { invoiceNumber: 'VLX-000042', amountLabel: '$300.00' }), {
    title: 'Plan changed',
    description: 'A new billing period started. Invoice VLX-000042 for $300.00 was created for it.',
  })
  // Written as a draft (collection paused, or tax not calculated yet): its
  // total may still change, so the toast gives none.
  assert.deepEqual(planChangeToast({ pending_plan_id: null }, undefined, { invoiceNumber: 'VLX-000042', amountLabel: '$300.00', draft: true }), {
    title: 'Plan changed',
    description: 'A new billing period started. Invoice VLX-000042 was created for it as a draft; its total may change before it is finalized.',
  })
})

// #1280: a change now to another billing interval is not a price difference
// for the rest of the period. A new period starts and is invoiced at once.
test('a change to another billing interval says a new period starts', () => {
  const advance = changePlanNowHelper('in_advance', 'yearly')
  assert.doesNotMatch(advance, /price difference|charged or credited now/)
  assert.match(advance, /A new yearly billing period starts now and is invoiced now/)
  assert.match(advance, /unused part of the current period is credited/)
  assert.match(changePlanNowHelper('in_advance', 'monthly'), /A new monthly billing period/)
  const arrears = changePlanNowHelper('in_arrears', 'yearly')
  assert.match(arrears, /current period ends now/)
  assert.doesNotMatch(arrears, /invoiced now|credited/)
  // Same interval: unchanged.
  assert.match(changePlanNowHelper('in_advance', undefined), /charged or credited now/)
})

test('remove card: only card, default with others left, non-default', () => {
  assert.equal(cardLabel({ card_brand: 'visa', card_last4: '4242' }), 'Visa ····4242')
  assert.equal(cardLabel({}), 'Card ····????')
  assert.match(removeCardSentence({ onlyCard: true, isDefault: true }), /only card/)
  assert.doesNotMatch(removeCardSentence({ onlyCard: true, isDefault: true }), /emailed/)
  assert.match(removeCardSentence({ onlyCard: false, isDefault: true }), /newest remaining card becomes the default/)
  assert.match(removeCardSentence({ onlyCard: false, isDefault: false }), /default card stays the same/)
})
