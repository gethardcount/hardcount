// What the Margin card can say about the cost side of a window.
//
// A margin is (usage charges − provider cost) / usage charges. With no
// provider cost recorded the formula reads 100%, a margin nobody measured.
// The server now leaves margin_bps out in that case and says why
// (costed_events, cost_rates_configured); this turns that into the one
// state the card renders.

export type MarginCostState =
  | 'measured'   // at least one event in the window carries a provider cost
  | 'no-rates'   // the workspace has no provider cost rate at all
  | 'no-match'   // rates exist, but none matched this usage
  | 'no-usage'   // nothing in the window to cost

export function marginCostState(rep: {
  revenue_cents: number
  costed_events?: number
  cost_rates_configured?: boolean
  unresolved_events: number
}): MarginCostState {
  if ((rep.costed_events ?? 0) > 0) return 'measured'
  if (rep.cost_rates_configured === false) return 'no-rates'
  if (rep.revenue_cents === 0 && rep.unresolved_events === 0) return 'no-usage'
  return 'no-match'
}

// marginCostNote is the sentence under the figures, or null when the cost
// side was measured.
export function marginCostNote(state: MarginCostState): string | null {
  switch (state) {
    case 'measured': return null
    case 'no-rates': return 'No provider costs configured, so there is no margin to show.'
    case 'no-match': return 'No provider rate matched this usage, so its cost and margin are unknown.'
    case 'no-usage': return null
  }
}
