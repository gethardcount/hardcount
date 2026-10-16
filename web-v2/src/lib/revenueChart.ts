// Zero-fill for the Dashboard revenue chart.
//
// GET /v1/analytics/revenue-chart groups paid invoices by bucket and returns
// only the buckets that had revenue (internal/analytics/revenue_chart.go).
// Plotted as-is on a category axis, payments on Sep 20 and Oct 15 sit next to
// each other and the area joins them, as if revenue was flat for 25 days.
// fillRevenueBuckets inserts a zero for every missing bucket in the window the
// server queried, so the gaps read as days (or months) with no revenue.
//
// The window mirrors analytics.parsePeriod: [now - N days, now) in UTC, with
// day buckets for 7d/30d/90d and month buckets for 12m (365 days). Bucket keys
// match the SQL's to_char format: "YYYY-MM-DD" or "YYYY-MM", in UTC (the
// date_trunc runs in the database session zone, UTC in the shipped config).

export interface RevenueBucket {
  date: string
  revenue_cents: number
  invoice_count: number
}

const PERIODS: Record<string, { days: number; month: boolean }> = {
  '7d': { days: 7, month: false },
  '30d': { days: 30, month: false },
  '90d': { days: 90, month: false },
  '12m': { days: 365, month: true },
}

const DAY_MS = 24 * 60 * 60 * 1000

function pad(n: number): string {
  return String(n).padStart(2, '0')
}

// fillRevenueBuckets returns one point per bucket from the window's first
// bucket through `nowMs`'s bucket, in order. Server points keep their values;
// missing buckets get zero. A server point outside the computed window (the
// browser's clock differs from the server's) is kept, so no revenue is
// dropped. An unknown period falls back to 30d, as parsePeriod does.
export function fillRevenueBuckets<T extends RevenueBucket>(
  data: T[],
  period: string,
  nowMs: number,
): RevenueBucket[] {
  const { days, month } = PERIODS[period] ?? PERIODS['30d']
  const start = new Date(nowMs - days * DAY_MS)
  const end = new Date(nowMs)
  const keys: string[] = []
  if (month) {
    let y = start.getUTCFullYear()
    let m = start.getUTCMonth()
    const endY = end.getUTCFullYear()
    const endM = end.getUTCMonth()
    while (y < endY || (y === endY && m <= endM)) {
      keys.push(`${y}-${pad(m + 1)}`)
      m++
      if (m === 12) { m = 0; y++ }
    }
  } else {
    let t = Date.UTC(start.getUTCFullYear(), start.getUTCMonth(), start.getUTCDate())
    const endDay = Date.UTC(end.getUTCFullYear(), end.getUTCMonth(), end.getUTCDate())
    for (; t <= endDay; t += DAY_MS) {
      const d = new Date(t)
      keys.push(`${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())}`)
    }
  }
  const byKey = new Map<string, RevenueBucket>()
  for (const k of keys) byKey.set(k, { date: k, revenue_cents: 0, invoice_count: 0 })
  for (const p of data) {
    byKey.set(p.date, { date: p.date, revenue_cents: p.revenue_cents, invoice_count: p.invoice_count })
  }
  return [...byKey.values()].sort((a, b) => (a.date < b.date ? -1 : a.date > b.date ? 1 : 0))
}
