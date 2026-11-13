// Save Changes with an invalid field on another tab did nothing: only the
// open tab is rendered, so its error was invisible. The page now opens the
// tab of the first invalid field and says what to fix.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { changedSettings, firstInvalidSettingsField, invalidSettingsToast } from '../src/lib/settingsFields.ts'

test('finds the first invalid field in page order, with its tab', () => {
  const f = firstInvalidSettingsField({
    tax_rate: { message: 'Enter a rate from 0 to 100' },
    net_payment_terms: { message: 'Enter a number of days from 0 to 365' },
  })
  assert.deepEqual(f, {
    field: 'net_payment_terms',
    tab: 'invoicing',
    label: 'Payment terms',
    message: 'Enter a number of days from 0 to 365',
  })
  assert.deepEqual(invalidSettingsToast(f!), {
    title: "Couldn't save settings",
    description: 'Payment terms on the Invoicing tab: Enter a number of days from 0 to 365.',
  })
})

test('no errors: nothing to show', () => {
  assert.equal(firstInvalidSettingsField({}), null)
  assert.equal(firstInvalidSettingsField({ company_name: undefined }), null)
})

test('an unmapped field still produces a message, so Save is never silent', () => {
  const f = firstInvalidSettingsField({ some_new_field: { message: '' } })
  assert.equal(f?.tab, 'general')
  assert.equal(f?.message, 'Check this value.')
})

test('the credit note prefix is on the Invoicing tab, next to the invoice prefix', () => {
  const f = firstInvalidSettingsField({ credit_note_prefix: { message: 'Letters, numbers, and hyphens only' } })
  assert.deepEqual(f, {
    field: 'credit_note_prefix',
    tab: 'invoicing',
    label: 'Credit note prefix',
    message: 'Letters, numbers, and hyphens only',
  })
})

// A Settings save sent every field, so a page opened before someone else's
// save put that save's tax rate back. It now sends only what changed here,
// and the server keeps the rest.
test('a save sends only the fields changed on the page', () => {
  const saved = { company_name: 'Acme', tax_rate: 8, tax_inclusive: false, credit_balance_low_threshold: '' }
  assert.deepEqual(changedSettings(saved, { ...saved, company_name: 'Acme GmbH' }), { company_name: 'Acme GmbH' })
  assert.deepEqual(changedSettings(saved, { ...saved }), {})
})

test('a change to 0, false or an empty value is still a change', () => {
  const saved = { tax_rate: 8, tax_inclusive: true, credit_balance_low_threshold: '50' }
  assert.deepEqual(
    changedSettings(saved, { tax_rate: 0, tax_inclusive: false, credit_balance_low_threshold: '' }),
    { tax_rate: 0, tax_inclusive: false, credit_balance_low_threshold: '' },
  )
})

test('with nothing loaded, every field is sent', () => {
  const edited = { company_name: 'Acme', tax_rate: 8 }
  assert.deepEqual(changedSettings(undefined, edited), edited)
})
