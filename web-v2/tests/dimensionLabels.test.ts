// A price's labels (its dimension_match) are one JSON object, so a second
// row with the same name replaced the first and the price saved with one
// label where two were typed. The form refuses that on the row.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { labelRowsToMatch } from '../src/lib/dimensionLabels.ts'

test('rows become the match, trimmed; empty rows are left out', () => {
  assert.deepEqual(labelRowsToMatch([]), { match: {} })
  assert.deepEqual(
    labelRowsToMatch([{ key: ' model ', value: ' gpt-4o ' }, { key: '', value: '' }, { key: 'token_type', value: 'input' }]),
    { match: { model: 'gpt-4o', token_type: 'input' } },
  )
})

test('a second row with the same name is refused on that row, never merged', () => {
  const r = labelRowsToMatch([{ key: 'model', value: 'gpt-4o' }, { key: 'model', value: 'gpt-4o-mini' }])
  assert.deepEqual(r, { errors: { 1: { key: '"model" is already label 1. A price matches each label name once.' } } })
  // Trimmed before the comparison, as before the save.
  assert.ok('errors' in labelRowsToMatch([{ key: 'model', value: 'a' }, { key: ' model', value: 'b' }]))
  // Names differ by case: the server compares them exactly, so both stand.
  assert.deepEqual(labelRowsToMatch([{ key: 'model', value: 'a' }, { key: 'Model', value: 'b' }]), { match: { model: 'a', Model: 'b' } })
})

test('a row with only a value or only a name is refused, not dropped', () => {
  assert.deepEqual(labelRowsToMatch([{ key: '', value: 'gpt-4o' }]), { errors: { 0: { key: 'Enter a label name, or remove this row.' } } })
  assert.deepEqual(labelRowsToMatch([{ key: 'model', value: '  ' }]), { errors: { 0: { value: 'Enter a value.' } } })
})
