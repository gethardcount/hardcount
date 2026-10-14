// Usage that matched no price on its meter. The server counts it in a
// separate "unmatched" row per meter and bills nothing for it
// (usage.CustomerUsageService.rateMeter). Its warning sentence names the
// meter key and engine words ("matching no pricing rule"), so the dashboard
// builds its own plain notice from the rows and keeps the server's words in
// Details. Plain data so node --test can check it.

export interface UnpricedMeter {
  meterId: string
  meterName: string
  unit: string
  quantity: number
}

export function unpricedMeters(
  meters: { meter_id: string; meter_name: string; meter_key: string; unit: string; rules: { unmatched?: boolean; quantity: string }[] }[],
): UnpricedMeter[] {
  const out: UnpricedMeter[] = []
  for (const m of meters) {
    const quantity = m.rules.filter(r => r.unmatched).reduce((sum, r) => sum + Number(r.quantity), 0)
    if (quantity > 0) out.push({ meterId: m.meter_id, meterName: m.meter_name || m.meter_key, unit: m.unit, quantity })
  }
  return out
}

export const UNPRICED_TITLE = 'Some usage has no price'
export const UNPRICED_SENTENCE = "Usage that matches no price isn't billed. Add a price for it on the meter."

// "1,250 tokens on Tokens". The quantity is exact (up to 4 decimals), never
// rounded to a friendlier number: it is the volume that is not billed.
export function unpricedLine(m: UnpricedMeter): string {
  const qty = m.quantity.toLocaleString('en-US', { maximumFractionDigits: 4 })
  return `${qty}${m.unit ? ` ${m.unit}` : ''} on ${m.meterName} not billed.`
}
