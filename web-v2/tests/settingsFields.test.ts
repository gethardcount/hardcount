// Save Changes with an invalid field on another tab did nothing: only the
// open tab is rendered, so its error was invisible. The page now opens the
// tab of the first invalid field and says what to fix.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { firstInvalidSettingsField, invalidSettingsToast } from '../src/lib/settingsFields.ts'

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
