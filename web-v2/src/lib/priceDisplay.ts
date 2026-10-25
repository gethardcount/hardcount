// Shared price-display helpers (extracted from the recipe preview, PR #608;
// promoted for the whole Pricing section, ADR-100 follow-up).
//
// Rates are exact decimal-string cents per unit. Display does decimal-point
// arithmetic on the STRING — never parseFloat — so sub-cent rates render
// exactly ("0.0000275" ×10⁴ → "0.275", never 0.27500000000000002), and a
// rate is never rounded: $0.125 stays $0.125. Whole-dollar values pad to
// money-standard two decimals ($5.00).
//
// Unit convention: token rates quote per 1M tokens (how every AI provider
// and rate card lists them); everything else quotes per single unit
// ("$0.0014 / second"). Currency comes from the ENTITY (rule/plan), never
// the tenant's active display default — post-ADR-100 every catalog object
// carries its own pinned currency.

import { getCurrencySymbol } from './api'

// PricedRule is the structural shape both the hand-rolled catalog RatingRule
// and the generated RecipeRatingRule satisfy.
export interface PricedRule {
  mode: string
  currency?: string
  unit_amount_cents?: string
  tiers?: { up_to?: number | null; unit_amount_cents?: string }[]
  package_size?: number
  package_amount_cents?: string
}

// PRICING_MODES are the price shapes, in the order a form offers them.
export const PRICING_MODES = ['per_unit', 'graduated', 'package'] as const

// pricingModeLabel names a price shape for display.
export function pricingModeLabel(mode: string): string {
  switch (mode) {
    case 'per_unit': return 'Per unit'
    case 'graduated': return 'Graduated tiers'
    case 'package': return 'Per package'
    default: return mode
  }
}

// shiftDecimal moves a decimal string's point right by `places` without
// float math.
export function shiftDecimal(s: string, places: number): string {
  const neg = s.startsWith('-')
  const [rawInt, rawFrac = ''] = (neg ? s.slice(1) : s).split('.')
  const digits = rawInt + rawFrac
  const point = rawInt.length + places
  let out: string
  if (point <= 0) {
    out = '0.' + '0'.repeat(-point) + digits
  } else if (point >= digits.length) {
    out = digits + '0'.repeat(point - digits.length)
  } else {
    out = digits.slice(0, point) + '.' + digits.slice(point)
  }
  out = out.replace(/^0+(?=\d)/, '')
  if (out.includes('.')) out = out.replace(/\.?0+$/, '')
  return (neg ? '-' : '') + (out === '' ? '0' : out)
}

// money renders a cents-decimal-string scaled by 10^shift with the entity's
// currency symbol — padded to two decimals, never rounded. An entity that
// carries no currency gets no symbol, never the workspace's.
export function money(cents: string, shift: number, currency: string | undefined): string {
  let v = shiftDecimal(cents, shift - 2)
  const frac = v.split('.')[1] ?? ''
  if (frac.length < 2) v = (frac.length === 0 ? v + '.' : v) + '0'.repeat(2 - frac.length)
  return (currency ? getCurrencySymbol(currency) : '') + v
}

// singular trims a plural unit for "per <unit>" copy (seconds → second).
export function singular(unit: string): string {
  return unit.length > 1 && unit.endsWith('s') ? unit.slice(0, -1) : unit
}

// unitScale gives the display scale for a meter unit: tokens per 1M,
// anything else per single unit.
export function unitScale(unit: string): { scale: number; per: string } {
  // Meters in the wild carry both 'tokens' and 'token' — same semantics.
  const perMillion = unit.trim().toLowerCase() === 'tokens' || unit.trim().toLowerCase() === 'token'
  return {
    scale: perMillion ? 6 : 0,
    per: perMillion ? '1M tokens' : singular(unit || 'unit'),
  }
}

// priceRate renders one rule's money in operator units, across all three
// pricing modes, in the rule's own currency. `unit` is the meter's unit
// when known ('' falls back to "per unit").
export function priceRate(r: PricedRule, unit: string): string {
  const { scale, per } = unitScale(unit)
  const cur = r.currency
  switch (r.mode) {
    case 'graduated': {
      const tiers = r.tiers ?? []
      if (tiers.length === 0) return `tiered / ${per}`
      const parts = tiers.map(t =>
        t.up_to != null
          ? `up to ${t.up_to.toLocaleString()} @ ${money(t.unit_amount_cents ?? '0', scale, cur)}`
          : `beyond @ ${money(t.unit_amount_cents ?? '0', scale, cur)}`,
      )
      return `tiered / ${per}: ${parts.join(', ')}`
    }
    case 'package':
      // A part package bills as a whole one, so there is nothing after it.
      return `${money(r.package_amount_cents ?? '0', 0, cur)} per ${(r.package_size ?? 0).toLocaleString()} ${unit || 'units'}`
    default:
      return `${money(r.unit_amount_cents ?? '0', scale, cur)} / ${per}`
  }
}

// invoiceLineRate renders an invoice line's unit price in operator units:
// token lines read "$3.00 / 1M tokens" (the same convention the Pricing
// section uses — unitScale above); everything else falls back to the exact
// per-unit rate string the caller supplies. `rate` is the already-formatted
// per-unit fallback (formatRate output) so non-scaled lines render byte-
// identically to before this helper existed.
export function invoiceLineRate(
  li: { unit_amount_decimal?: string; meter_unit?: string },
  currency: string | undefined,
  fallback: string,
): string {
  if (!li.meter_unit || !li.unit_amount_decimal) return fallback
  const { scale, per } = unitScale(li.meter_unit)
  if (scale === 0) return fallback
  return `${money(li.unit_amount_decimal, scale, currency)} / ${per}`
}
