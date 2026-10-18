// The per-meter summary rows on the Usage page. Each meter is shown in its
// own unit and under its own aggregation, because adding tokens, calls and
// GB together (the old "Total Units") or summing a Maximum meter's readings
// (Storage 140 + 210 + 120 GB = "470 GB") contradicts what is billed.
//
// The server computes the value (GET /v1/usage-events/aggregate,
// by_meter[].value); these helpers only name it and draw its daily shape.

import { meterAggregationLabel } from './meterLabels'

// meterValueCaption says what the number in a summary row is, so a peak is
// never read as a total. `customers` is how many customers reported on the
// meter in the window; max and last add one reading per customer.
export function meterValueCaption(aggregation: string, customers: number): string {
  const across = customers > 1 ? `, added across ${customers.toLocaleString('en-US')} customers` : ''
  switch (aggregation) {
    case 'count':
      return 'Number of events'
    case 'max':
      return `Peak reading${customers > 1 ? ' per customer' : ''}${across}`
    case 'last':
      return `Latest reading${customers > 1 ? ' per customer' : ''}${across}`
    case 'sum':
      return 'Sum of reported values'
    case 'mixed':
      return "Sum of reported values. This meter is billed in parts, one per pricing rule, that count usage in different ways or each take their own peak or latest reading, so the billed amount may differ."
    default:
      return 'Sum of reported values (aggregation unknown)'
  }
}

// meterModeBadge is the short aggregation label beside the meter name.
export function meterModeBadge(aggregation: string): string {
  if (aggregation === 'mixed') return 'Mixed'
  return aggregation ? meterAggregationLabel(aggregation) : 'Sum'
}

export interface DailyPoint {
  date: string
  value: string
}

// addDays steps a YYYY-MM-DD civil date. Uses UTC arithmetic on the civil
// date only, so the result never depends on the browser's timezone.
export function addDays(date: string, n: number): string {
  const [y, m, d] = date.split('-').map(Number)
  const t = new Date(Date.UTC(y, m - 1, d + n))
  return t.toISOString().slice(0, 10)
}

// fillDays turns the server's sparse daily series (days with events only)
// into one number per calendar day from `from` to `to` inclusive, with 0
// for quiet days. A window with no end date ("Last 30 days", All time)
// ends on `today` (the tenant's civil date), so quiet recent days show as
// zero instead of the line stopping at the last event; a later event day
// still extends it. Without `from` it starts on the first day with events.
// Capped at `maxDays` most recent days so a multi-year window still draws
// a readable line.
export function fillDays(daily: DailyPoint[], from?: string, to?: string, today?: string, maxDays = 366): number[] {
  if (daily.length === 0 && (!from || !(to || today))) return []
  const sorted = [...daily].sort((a, b) => a.date.localeCompare(b.date))
  let start = from || sorted[0]?.date
  const lastEvent = sorted[sorted.length - 1]?.date
  const end = to || [today, lastEvent].filter((d): d is string => !!d).sort().pop()
  if (!start || !end || start > end) return []
  const byDate = new Map(sorted.map(p => [p.date, Number(p.value) || 0]))
  // Cap from the end: the newest days matter most on a sparkline.
  let span = 0
  for (let d = start; d <= end && span <= maxDays; d = addDays(d, 1)) span++
  if (span > maxDays) start = addDays(end, -(maxDays - 1))
  const out: number[] = []
  for (let d = start; d <= end; d = addDays(d, 1)) out.push(byDate.get(d) ?? 0)
  return out
}

// sparklinePoints maps values to an SVG polyline in a w×h box (y grows
// down). The baseline is 0, so a steady non-zero series draws along the
// top and an all-zero series along the bottom.
export function sparklinePoints(values: number[], w: number, h: number): string {
  if (values.length === 0) return ''
  const max = Math.max(...values)
  const min = Math.min(0, ...values)
  const range = max - min
  const stepX = values.length > 1 ? w / (values.length - 1) : 0
  return values
    .map((v, i) => {
      const x = values.length > 1 ? i * stepX : w / 2
      const y = range === 0 ? h : h - ((v - min) / range) * h
      return `${round(x)},${round(y)}`
    })
    .join(' ')
}

function round(n: number): number {
  return Math.round(n * 10) / 10
}
