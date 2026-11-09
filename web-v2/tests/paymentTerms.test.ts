// The payment terms New invoice offers. It starts at the account's own terms
// (Settings, Payment terms), so they must be one of the choices, also when no
// preset has them: a Net 15 or Net 45 account would otherwise open at a
// choice the picker cannot show.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { paymentTermOptions } from '../src/lib/paymentTerms.ts'

const labels = (accountDays: number) => paymentTermOptions(accountDays).map(o => o.label)
const PRESETS = ['Due on receipt', 'Net 7', 'Net 14', 'Net 30', 'Net 60']

test("an account on a preset's terms gets the presets once each", () => {
  assert.deepEqual(labels(30), PRESETS)
  assert.deepEqual(labels(0), PRESETS)
})

test("the account's own terms are added in order of days when no preset has them", () => {
  assert.deepEqual(labels(15), ['Due on receipt', 'Net 7', 'Net 14', 'Net 15', 'Net 30', 'Net 60'])
  assert.deepEqual(labels(45), ['Due on receipt', 'Net 7', 'Net 14', 'Net 30', 'Net 45', 'Net 60'])
  assert.deepEqual(labels(90), [...PRESETS, 'Net 90'])
})

test('each choice carries its days as the picker value', () => {
  assert.deepEqual(paymentTermOptions(15).find(o => o.label === 'Net 15'), { value: '15', label: 'Net 15' })
  assert.deepEqual(paymentTermOptions(30)[0], { value: '0', label: 'Due on receipt' })
})
