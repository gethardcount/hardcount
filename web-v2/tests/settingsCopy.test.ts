// Settings copy that said something the code does not do (sweep C7, C9, C10,
// 2026-10-21): the business email hint promised a Reply-To that no email
// sets; the test-mode banner hid that settings are shared with live; and the
// invoice-prefix preview always showed -000001.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { COMPANY_EMAIL_HINT, sharedSettingsNotice, invoiceNumberPreview } from '../src/lib/settingsCopy.ts'

test('the business email hint names where the address is used, not a Reply-To', () => {
  assert.equal(COMPANY_EMAIL_HINT, 'Printed on invoices and credit notes. Payment pages link to it when no support URL is set.')
  assert.doesNotMatch(COMPANY_EMAIL_HINT, /reply/i)
})

test('test mode says the settings also change live billing; live mode needs no notice', () => {
  assert.equal(sharedSettingsNotice(false), 'Test and live mode share these settings, so a change here also applies to live invoices.')
  assert.equal(sharedSettingsNotice(true), null)
})

test('the prefix preview shows the real next number for the mode, and says the count carries on', () => {
  assert.equal(
    invoiceNumberPreview('ACME', 43, false),
    'Next test invoice: ACME-000043. Changing the prefix keeps the count.',
  )
  assert.equal(
    invoiceNumberPreview('ACME', 1, true),
    'Next live invoice: ACME-000001. Changing the prefix keeps the count.',
  )
  // Six digits is a minimum, as on the server (padSeq), not a cap.
  assert.equal(
    invoiceNumberPreview('INV', 1234567, true),
    'Next live invoice: INV-1234567. Changing the prefix keeps the count.',
  )
})
