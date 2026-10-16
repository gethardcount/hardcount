// Pins that the Margin card never shows a margin nobody measured. The defect:
// with no provider cost rates, the card read "Provider cost $0.0000 · Margin
// 100.0%" for every customer with usage.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { marginCostNote, marginCostState } from '../src/lib/marginCost.ts'

test('usage but no rates at all → no-rates, with the setup sentence', () => {
  const s = marginCostState({ revenue_cents: 19500, costed_events: 0, cost_rates_configured: false, unresolved_events: 0 })
  assert.equal(s, 'no-rates')
  assert.match(marginCostNote(s)!, /No provider costs configured/)
})

test('rates exist but none matched → no-match', () => {
  assert.equal(marginCostState({ revenue_cents: 4500, costed_events: 0, cost_rates_configured: true, unresolved_events: 3 }), 'no-match')
})

test('one costed event (even at zero cost) → measured, no note', () => {
  const s = marginCostState({ revenue_cents: 4500, costed_events: 1, cost_rates_configured: true, unresolved_events: 0 })
  assert.equal(s, 'measured')
  assert.equal(marginCostNote(s), null)
})

test('an empty window with rates is just empty', () => {
  assert.equal(marginCostState({ revenue_cents: 0, costed_events: 0, cost_rates_configured: true, unresolved_events: 0 }), 'no-usage')
})
