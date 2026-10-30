// dollarsToRateCents converts an operator-entered dollar amount into decimal
// cents as a string, preserving sub-cent precision (e.g. "0.000003" dollars →
// "0.0003" cents). Per-unit rates can be fractions of a cent, so we must not
// round to whole cents. We shift the decimal point two places right via string
// math rather than multiplying by 100 in floating point, which would introduce
// artifacts like 0.000003 * 100 = 0.00030000000000000003.
//
// Returns null when the input is blank or is not a plain decimal number. The
// server accepts 0 as a deliberate free rate, so reading a blank box as "0"
// would save a free price nobody typed. Callers show an error on null; an
// explicit "0" still returns "0".
//
// This module imports nothing so the unit tests can load it
// (tests/priceInput.test.ts).
export function dollarsToRateCents(input: string): string | null {
  const m = /^(-?)(\d*)(?:\.(\d*))?$/.exec(input.trim())
  // The pattern alone also matches "", "." and "-": require at least one digit.
  if (!m || (!m[2] && !m[3])) return null
  const sign = m[1]
  let intPart = m[2] || '0'
  let frac = m[3] || ''
  // Shift two places right: borrow up to two digits from the fraction.
  intPart += frac.slice(0, 2).padEnd(2, '0')
  frac = frac.slice(2)
  intPart = intPart.replace(/^0+(?=\d)/, '')
  let out = frac ? `${intPart}.${frac}` : intPart
  out = out.replace(/(\.\d*?)0+$/, '$1').replace(/\.$/, '')
  return out === '0' || out === '' ? '0' : `${sign}${out}`
}

// dollarsToWholeCents converts an amount typed in currency units ("25",
// "25.50") into whole cents, for fields that take money, not a rate: an
// invoice line's unit price, credits sold. It returns null for a blank box,
// a negative, a comma ("9,50": the box never guesses which one you meant),
// more than two decimals, or an amount too large to hold exactly. String
// math, like dollarsToRateCents, so 0.29 is 29 cents, never 28.999….
export function dollarsToWholeCents(input: string): number | null {
  const m = /^(\d*)(?:\.(\d{0,2}))?$/.exec(input.trim())
  if (!m || (!m[1] && !m[2])) return null
  const whole = m[1] || '0'
  if (whole.replace(/^0+/, '').length > 13) return null
  return Number(whole) * 100 + Number((m[2] || '').padEnd(2, '0'))
}

// wholeUnits reads a unit count typed in a box (a tier's "up to", a package
// size): digits only and greater than 0, or null. parseInt read "" as 0,
// "1000.5" as 1000 and "1e3" as 1, and the form then saved those.
export function wholeUnits(input: string): number | null {
  const t = input.trim()
  if (!/^\d+$/.test(t)) return null
  const n = Number(t)
  return n > 0 && Number.isSafeInteger(n) ? n : null
}

// amountInputError says why a typed amount was refused, for the line under
// its box. `example` is a valid amount for that box ("25.50"). `wholeCents`
// is set for an amount in whole cents (dollarsToWholeCents), which takes at
// most two decimals; a per-unit rate takes more.
export function amountInputError(input: string, example: string, wholeCents: boolean): string {
  const t = input.trim()
  if (t === '') return `Enter an amount, for example ${example}.`
  if (t.includes(',')) return `Use a point for decimals, not a comma: for example ${example}.`
  if (t.startsWith('-')) return "Enter 0 or more. An amount can't be negative."
  if (wholeCents && /^\d*\.\d{3,}$/.test(t)) return `Enter at most two decimals, for example ${example}.`
  if (wholeCents && /^0*[1-9]\d{13,}(\.\d*)?$/.test(t)) return 'That amount is too large.'
  return `Enter a plain number, for example ${example}.`
}

// wholeNumberIn reads a whole number typed in a box that takes a range, 0
// included where the field allows it (trial days, 0 to 730): digits only and
// from min to max, or null. parseInt cut "2.7" to 2; a blank box is not 0;
// "1e3" is not 1000.
export function wholeNumberIn(input: string, min: number, max: number): number | null {
  const t = input.trim()
  if (!/^\d+$/.test(t)) return null
  const n = Number(t)
  return Number.isSafeInteger(n) && n >= min && n <= max ? n : null
}

// percentRate reads a percent typed in a box ("8.875" for 8.875%): from 0 to
// 100 with at most four decimals, the precision the server stores, or null.
// A blank box is not 0: a rate of 0 turns tax off, so it must be typed.
export function percentRate(input: string): number | null {
  const m = /^(\d*)(?:\.(\d{0,4}))?$/.exec(input.trim())
  if (!m || (!m[1] && !m[2])) return null
  const n = Number(`${m[1] || '0'}.${m[2] || '0'}`)
  return n <= 100 ? n : null
}
