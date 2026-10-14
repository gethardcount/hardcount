// Usage quantities as the Usage Events page shows them. The server sends a
// quantity as a decimal string (NUMERIC(38,12)), so it is formatted as text,
// never through a float: "1234567.000000000000" reads "1,234,567" and
// "0.000000000001" stays exact.

// formatQuantity adds thousands separators to the whole part and drops
// trailing zeros from the fraction. Anything that isn't a plain decimal is
// returned as it came.
export function formatQuantity(raw: string): string {
  const m = /^(-?)(\d+)(?:\.(\d*))?$/.exec(raw.trim())
  if (!m) return raw
  const [, sign, whole, frac = ''] = m
  const grouped = whole.replace(/^0+(?=\d)/, '').replace(/\B(?=(\d{3})+(?!\d))/g, ',')
  const fraction = frac.replace(/0+$/, '')
  const out = fraction ? `${grouped}.${fraction}` : grouped
  return out === '0' || out === '' ? '0' : sign + out
}

// unitFor names a meter's unit for a quantity: "1 call", "2 calls". A meter
// unit is free text the operator typed ("call", "token", "GB", "tokens"),
// so only a lower-case word gets a plural ending; an abbreviation ("GB") or
// a unit already ending in s is left alone.
export function unitFor(quantity: string, unit: string): string {
  const u = unit.trim()
  if (!u) return ''
  const n = Number(quantity)
  if (n === 1 || !/[a-z]$/.test(u) || /s$/.test(u)) return u
  if (/[^aeiou]y$/.test(u)) return u.slice(0, -1) + 'ies'
  if (/(ch|sh|x|z)$/.test(u)) return u + 'es'
  return u + 's'
}

// quantityWithUnit reads "12,500 tokens", or just the number with no unit.
export function quantityWithUnit(raw: string, unit: string): string {
  const u = unitFor(raw, unit)
  return u ? `${formatQuantity(raw)} ${u}` : formatQuantity(raw)
}
