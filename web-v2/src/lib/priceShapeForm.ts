import type { PriceShape } from './api'
import { amountInputError, dollarsToRateCents, wholeUnits } from './priceInput'

// The price-shape form's values, as typed: a mode and the boxes that mode
// uses. Rates are in currency units and go out as decimal cents
// (dollarsToRateCents), so sub-cent rates stay exact.
export interface PriceShapeInput {
  mode: string
  unitAmount: string
  tiers: { upTo: string; price: string }[]
  packageSize: string
  packageAmount: string
}

// The box an error belongs under. `tier` is the tier's index for the tier
// boxes.
export type ShapeField = 'unitAmount' | 'tierUpTo' | 'tierPrice' | 'packageSize' | 'packageAmount'
export interface ShapeError { field: ShapeField; tier?: number; message: string }

// rate reads one rate box: decimal cents, or the reason it is refused. The
// server accepts 0 as a deliberate free rate, so a blank box is refused here:
// the server cannot tell it from a typed 0. A negative is refused here too.
function rate(input: string, example: string): { cents: string } | { message: string } {
  const cents = dollarsToRateCents(input)
  if (cents === null || cents.startsWith('-')) {
    const why = amountInputError(input, example, false)
    return { message: input.trim() === '' ? `${why} Use 0 for free.` : why }
  }
  return { cents }
}

// buildPriceShape turns the form into the request's shape, or the error to
// show under the box that is wrong. Nothing is rounded or filled in.
export function buildPriceShape(f: PriceShapeInput): { shape: PriceShape } | { error: ShapeError } {
  if (f.mode === 'per_unit') {
    const r = rate(f.unitAmount, '0.01')
    if ('message' in r) return { error: { field: 'unitAmount', message: r.message } }
    return { shape: { mode: 'per_unit', unit_amount_cents: r.cents } }
  }
  if (f.mode === 'graduated') {
    const { tiers } = f
    const out: { up_to: number | null; unit_amount_cents: string }[] = []
    let prev = 0
    for (let i = 0; i < tiers.length; i++) {
      // The last tier has no upper bound: up_to null.
      let upTo: number | null = null
      if (i < tiers.length - 1) {
        upTo = wholeUnits(tiers[i].upTo)
        if (upTo === null) return { error: { field: 'tierUpTo', tier: i, message: `Tier ${i + 1}: enter how many units the tier goes up to, a whole number greater than 0.` } }
        if (upTo <= prev) return { error: { field: 'tierUpTo', tier: i, message: `Tier ${i + 1}: "Up to" must be greater than ${prev}.` } }
        prev = upTo
      }
      const r = rate(tiers[i].price, '0.01')
      if ('message' in r) return { error: { field: 'tierPrice', tier: i, message: r.message } }
      out.push({ up_to: upTo, unit_amount_cents: r.cents })
    }
    return { shape: { mode: 'graduated', tiers: out } }
  }
  const size = wholeUnits(f.packageSize)
  if (size === null) return { error: { field: 'packageSize', message: 'Enter a whole number greater than 0, for example 100.' } }
  const r = rate(f.packageAmount, '10.00')
  if ('message' in r) return { error: { field: 'packageAmount', message: r.message } }
  return { shape: { mode: 'package', package_size: size, package_amount_cents: r.cents } }
}
