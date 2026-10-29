// The plan rate card: one label across columns, the rest as row headings,
// or no card when the prices don't fit a grid.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { buildRateCard, rowMatches } from '../src/lib/rateCard.ts'

const p = (m: Record<string, string>) => ({ dimension_match: m })

test('token_type goes across columns and model down the rows', () => {
  const prices = [
    p({ model: 'opus', token_type: 'input' }), p({ model: 'opus', token_type: 'output' }),
    p({ model: 'haiku', token_type: 'input' }), p({ model: 'haiku', token_type: 'output' }),
    p({ model: 'sonnet', token_type: 'input' }), p({ model: 'sonnet', token_type: 'output' }),
  ]
  const card = buildRateCard(prices)!
  assert.equal(card.columnKey, 'token_type')
  assert.deepEqual(card.columns, ['input', 'output'])
  assert.deepEqual(card.rowKeys, ['model'])
  assert.deepEqual(card.rows.map(r => r.labels.model), ['opus', 'haiku', 'sonnet'])
  assert.equal(card.rows[1].cells.output, prices[3])
})

test('a label on only some prices becomes its own row heading', () => {
  const card = buildRateCard([
    p({ model: 'gemini', context_tier: 'up_to_200k', token_type: 'input' }),
    p({ model: 'gemini', context_tier: 'up_to_200k', token_type: 'output' }),
    p({ model: 'gemini', context_tier: 'over_200k', token_type: 'input' }),
    p({ model: 'gemini', context_tier: 'over_200k', token_type: 'output' }),
    p({ model: 'opus', token_type: 'input' }), p({ model: 'opus', token_type: 'output' }),
  ])!
  assert.equal(card.columnKey, 'token_type')
  assert.deepEqual(card.rowKeys, ['model', 'context_tier'])
  assert.deepEqual(card.rows.map(r => r.labels), [
    { model: 'gemini', context_tier: 'up_to_200k' }, { model: 'gemini', context_tier: 'over_200k' }, { model: 'opus' },
  ])
})

test('no card when nothing fits a grid', () => {
  assert.equal(buildRateCard([p({})]), null, 'one price')
  assert.equal(buildRateCard([p({}), p({ region: 'eu' })]), null, 'no label on every price')
  // Two prices in one cell.
  assert.equal(buildRateCard([p({ model: 'a', t: 'in' }), p({ model: 'a', t: 'in', tier: 'x' }), p({ model: 'b', t: 'out' })]), null)
  // Every row would hold one cell: a list, not a card.
  assert.equal(buildRateCard([p({ region: 'eu', model: 'a' }), p({ region: 'us', model: 'b' })]), null)
  // Too many values to be columns.
  const many = Array.from({ length: 9 }, (_, i) => p({ model: 'm', size: `s${i}` }))
  assert.equal(buildRateCard(many), null)
})

test('a short list or a tiered price stays a list', () => {
  // One model priced for input and output: two rows of a list read fine.
  assert.equal(buildRateCard([p({ token_type: 'input' }), p({ token_type: 'output' }), p({ token_type: 'cache_read' })]), null)
  // A tier table does not fit in a cell.
  const tiered = [
    { dimension_match: { model: 'a', t: 'in' }, mode: 'graduated' }, { dimension_match: { model: 'a', t: 'out' }, mode: 'per_unit' },
    { dimension_match: { model: 'b', t: 'in' }, mode: 'per_unit' }, { dimension_match: { model: 'b', t: 'out' }, mode: 'per_unit' },
  ]
  assert.equal(buildRateCard(tiered), null)
  assert.notEqual(buildRateCard(tiered.map(x => ({ ...x, mode: 'per_unit' }))), null, 'control: all per unit')
})

test('search matches every word in the labels or the meter name', () => {
  assert.equal(rowMatches({ model: 'claude-sonnet-4.5' }, 'Tokens', 'sonnet'), true)
  assert.equal(rowMatches({ model: 'claude-sonnet-4.5' }, 'Tokens', 'SONNET 4.5'), true)
  assert.equal(rowMatches({ model: 'claude-sonnet-4.5' }, 'Tokens', 'opus'), false)
  assert.equal(rowMatches({ model: 'x' }, 'Tokens', 'tokens'), true)
  assert.equal(rowMatches({ model: 'x' }, 'Tokens', '  '), true)
})
