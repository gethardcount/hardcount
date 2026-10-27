// Usage that matched no price on its meter. The server counts it in a
// separate no_price row per meter and plan and bills nothing for it.
// Its warning sentence names the meter's event name, so the dashboard
// builds its own plain notice from the rows and keeps the server's words in
// Details. Plain data so the unit tests can check it.

export interface UnpricedMeter {
  meterId: string
  meterName: string
  unit: string
  quantity: number
}

export function unpricedMeters(
  meters: { meter_id: string; meter_name: string; event_name: string; unit: string; prices: { no_price?: boolean; quantity: string }[] }[],
): UnpricedMeter[] {
  const out: UnpricedMeter[] = []
  for (const m of meters) {
    const quantity = m.prices.filter(r => r.no_price).reduce((sum, r) => sum + Number(r.quantity), 0)
    if (quantity > 0) out.push({ meterId: m.meter_id, meterName: m.meter_name || m.event_name, unit: m.unit, quantity })
  }
  return out
}

export const UNPRICED_TITLE = 'Some usage has no price'
export const UNPRICED_SENTENCE = "Usage that matches no price isn't billed. Add a price for it on the plan."

// "1,250 tokens on Tokens". The quantity is exact (up to 4 decimals), never
// rounded to a friendlier number: it is the volume that is not billed.
export function unpricedLine(m: UnpricedMeter): string {
  const qty = m.quantity.toLocaleString('en-US', { maximumFractionDigits: 4 })
  return `${qty}${m.unit ? ` ${m.unit}` : ''} on ${m.meterName} not billed.`
}

// The quantity line under a meter's name on the cost dashboard. The meter's
// total_quantity counts only billed usage (usage with no price stays out of
// the totals so they match the invoice), so a meter whose usage all matched no
// price used to read "0 tokens · 0 rules" right under a notice saying 12,400
// tokens were not billed. Say both numbers, in the notice's words:
//   "12,400 tokens"
//   "12,400 tokens · 3 prices"
//   "0 tokens billed · 12,400 tokens not billed · no price matched"
export function meterQuantityLine(m: {
  unit: string
  total_quantity: string
  prices: { no_price?: boolean; quantity: string }[]
}): string {
  const qty = (n: number) => `${n.toLocaleString('en-US', { maximumFractionDigits: 4 })}${m.unit ? ` ${m.unit}` : ''}`
  const billed = Number(m.total_quantity)
  const notBilled = m.prices.filter(r => r.no_price).reduce((sum, r) => sum + Number(r.quantity), 0)
  const prices = m.prices.filter(r => !r.no_price).length
  const parts = notBilled > 0 ? [`${qty(billed)} billed`, `${qty(notBilled)} not billed`] : [qty(billed)]
  if (notBilled > 0 || prices > 1) {
    parts.push(prices === 0 ? 'no price matched' : `${prices} price${prices === 1 ? '' : 's'}`)
  }
  return parts.join(' · ')
}
