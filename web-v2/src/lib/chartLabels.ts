import { formatInTimeZone } from 'date-fns-tz'

// Chart bucket labels.
//
// The server buckets chart data by UTC calendar day (or month) and sends each
// bucket's key as that day's NAME: the revenue chart sends "2026-10-15" (or
// "2026-10" on the 12-month tab), the customer usage chart sends the UTC
// midnight that starts the day ("2026-10-01T00:00:00Z"). A key names a
// calendar day; it is not a moment to convert. Projecting it into the tenant
// timezone moved every label one day back west of UTC: bucket 2026-10-15 read
// "Oct 14" in New York, and the 12-month bucket "2026-10" read "Sep 30".
//
// So a bucket is labelled by its own UTC calendar date. Which days the server
// groups by is a separate decision; these helpers only name what it sent.

// utcBucketLabel renders a bucket key ("YYYY-MM-DD", "YYYY-MM" or a UTC-midnight
// ISO instant) with a date-fns pattern, reading the date in UTC. Unparseable
// input is returned unchanged so a bad key shows as itself, not "Invalid Date".
export function utcBucketLabel(key: string, pattern: string): string {
  const d = new Date(key)
  if (Number.isNaN(d.getTime())) return key
  return formatInTimeZone(d, 'UTC', pattern)
}

// revenueBucketLabel labels a revenue-chart bucket: a month key ("2026-10")
// reads "Oct 2026", a day key ("2026-10-15") reads "Oct 15".
export function revenueBucketLabel(key: string): string {
  return utcBucketLabel(key, /^\d{4}-\d{2}$/.test(key) ? 'MMM yyyy' : 'MMM d')
}

// compactAxisTick labels a quantity axis tick short enough for a 40px axis:
// 180000 reads "180K". The full number cut off at the axis's edge, so 180000
// and 135000 read "80000" and "35000" on the customer's Daily usage chart.
const compact = new Intl.NumberFormat('en-US', { notation: 'compact', maximumFractionDigits: 1 })
export function compactAxisTick(value: number): string {
  return compact.format(value)
}
