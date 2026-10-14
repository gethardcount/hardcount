import { formatCents } from './money'

// The worked example under Settings → Tax (manual rate). Every amount in it
// is formatted the same way, in the workspace currency: the "100.00" used to
// take its symbol from the currency picker list while the result used
// formatCents, so a CAD workspace read "$100.00 … = CA$118.00".
//
// The math matches the existing example: exclusive adds round(base × rate),
// inclusive carves the tax out of the base. Amounts are whole cents.
export function taxExampleAmounts(
  ratePercent: number,
  inclusive: boolean,
  currency: string,
): { base: string; net: string; total: string } {
  const base = 10000
  const bp = ratePercent * 100
  const net = inclusive ? Math.round((base * 10000) / (10000 + bp)) : base
  const total = inclusive ? base : base + Math.round((base * bp) / 10000)
  return {
    base: formatCents(base, currency),
    net: formatCents(net, currency),
    total: formatCents(total, currency),
  }
}
