// A plan's usage prices as a rate card: per meter, one label spread across
// columns (token_type on an AI plan: input, output, cache_read …) and the
// other labels as row headings (model, context_tier). An AI plan's 125 prices
// read as about 30 rows instead of 125 stacked blocks.
//
// The column label is chosen among the labels every price of the meter
// carries with 2 to 8 values (more would not fit as columns): the one with
// the fewest values, then the one giving the fewest rows, then the first
// seen. A
// label that would put two prices in one cell, or leave one cell per row,
// can't be the columns. With none left there is no card, and the caller lists
// the prices one per row. So does a meter with a graduated or package price
// (a tier table does not fit in a cell) or one that would give a single row
// (two or three prices read fine as a list). Orders follow the prices' own
// order, so a recipe's model order is kept.

export interface RateCardRow<P> {
  labels: Record<string, string>
  cells: Record<string, P>
}

export interface RateCard<P> {
  rowKeys: string[]
  columnKey: string
  columns: string[]
  rows: RateCardRow<P>[]
}

const MIN_COLUMNS = 2
const MAX_COLUMNS = 8

export function buildRateCard<P extends { dimension_match?: Record<string, string> | null; mode?: string }>(prices: P[]): RateCard<P> | null {
  if (prices.length < 3) return null
  if (prices.some(p => p.mode !== undefined && p.mode !== 'per_unit')) return null
  const keyOrder: string[] = []
  const values = new Map<string, string[]>()
  for (const p of prices) {
    for (const [k, v] of Object.entries(p.dimension_match ?? {})) {
      if (!values.has(k)) { values.set(k, []); keyOrder.push(k) }
      const vs = values.get(k)!
      if (!vs.includes(v)) vs.push(v)
    }
  }
  let best: RateCard<P> | null = null
  for (const k of keyOrder) {
    const n = values.get(k)!.length
    if (n < MIN_COLUMNS || n > MAX_COLUMNS) continue
    if (!prices.every(p => (p.dimension_match ?? {})[k] !== undefined)) continue
    const card = pivot(prices, keyOrder, k, values.get(k)!)
    if (!card) continue
    if (!best || card.columns.length < best.columns.length || (card.columns.length === best.columns.length && card.rows.length < best.rows.length)) best = card
  }
  return best
}

function pivot<P extends { dimension_match?: Record<string, string> | null }>(prices: P[], keyOrder: string[], columnKey: string, columns: string[]): RateCard<P> | null {
  const rowKeys = keyOrder.filter(k => k !== columnKey)
  const rows: RateCardRow<P>[] = []
  const byRow = new Map<string, RateCardRow<P>>()
  for (const p of prices) {
    const m = p.dimension_match ?? {}
    const labels: Record<string, string> = {}
    for (const k of rowKeys) if (m[k] !== undefined) labels[k] = m[k]
    const id = JSON.stringify(rowKeys.map(k => m[k] ?? null))
    let row = byRow.get(id)
    if (!row) { row = { labels, cells: {} }; byRow.set(id, row); rows.push(row) }
    const col = m[columnKey]
    if (row.cells[col] !== undefined) return null
    row.cells[col] = p
  }
  // One cell per row is a list in disguise; one row is a short list.
  if (rows.length === prices.length || rows.length < 2) return null
  return { rowKeys, columnKey, columns, rows }
}

// rowMatches is the card's search: a row matches when any of its labels, or
// the meter name, contains every word typed (case-insensitive).
export function rowMatches(labels: Record<string, string>, meterName: string, query: string): boolean {
  const words = query.toLowerCase().split(/\s+/).filter(Boolean)
  if (words.length === 0) return true
  const hay = [meterName, ...Object.values(labels)].join(' ').toLowerCase()
  return words.every(w => hay.includes(w))
}
