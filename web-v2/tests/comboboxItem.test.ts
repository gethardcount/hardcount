// Pins how a picker's options become cmdk items. cmdk highlights every item
// whose value equals the highlighted value, and Enter picks the first of
// them. Two customers with the same name must still be two items, or a
// keyboard pick of the second sends the first customer's id.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { comboboxFilter, comboboxItem } from '../src/lib/comboboxItem.ts'

const twins = [
  { value: 'hc_cus_1', label: 'Acme Corp' },
  { value: 'hc_cus_2', label: 'Acme Corp' },
  { value: 'hc_cus_3', label: 'Bolt Ltd' },
]

// The option a keyboard user gets after pressing ArrowDown `downs` times and
// then Enter, by cmdk 1.1's rules. The highlighted item is the first one whose
// value equals the highlighted value. ArrowDown highlights the value of the
// item after it, and Enter picks the highlighted item.
function keyboardPick(options: { value: string; label: string }[], downs: number): string {
  const values = options.map(o => comboboxItem(o).value)
  let highlighted = values[0]
  for (let i = 0; i < downs; i++) {
    const at = values.indexOf(highlighted)
    if (at + 1 < values.length) highlighted = values[at + 1]
  }
  return options[values.indexOf(highlighted)].value
}

test('a keyboard pick of the second of two same-named customers picks the second', () => {
  assert.equal(keyboardPick(twins, 1), 'hc_cus_2')
  assert.equal(keyboardPick(twins, 2), 'hc_cus_3')
})

test('every option gets its own cmdk value, which cmdk can highlight', () => {
  const options = [...twins, { value: '', label: 'No test clock — bills in real time' }]
  const values = options.map(o => comboboxItem(o).value)
  assert.equal(new Set(values).size, options.length, `values ${JSON.stringify(values)} repeat`)
  for (const v of values) {
    // cmdk trims each value, and never highlights or matches an empty one.
    assert.notEqual(v, '')
    assert.equal(v, v.trim())
  }
})

test('search finds an option by its name, its value or a keyword, in any case', () => {
  const matches = (option: { value: string; label: string; keywords?: string[] }, search: string) => {
    const item = comboboxItem(option)
    return comboboxFilter(item.value, search, item.keywords) === 1
  }
  const germany = { value: 'DE', label: 'Germany', keywords: ['Deutschland'] }
  assert.ok(matches(germany, 'germ'))
  assert.ok(matches(germany, 'de'))
  assert.ok(matches(germany, 'DEUTSCH'))
  assert.ok(!matches(germany, 'france'))
  assert.ok(matches(twins[1], 'acme corp'))
  assert.ok(matches(twins[1], 'hc_cus_2'))
  assert.ok(!matches(twins[0], 'hc_cus_2'))
})
