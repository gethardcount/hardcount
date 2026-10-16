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
