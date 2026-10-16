import { format } from 'date-fns'
import { formatInTimeZone, fromZonedTime } from 'date-fns-tz'
import { getTenantTimezone } from '@/lib/api'
import type { EffectiveNow } from './effectiveNow'

// Date helpers that interpret operator-picked civil dates in tenant
// timezone, returning UTC ISO 8601 timestamps suitable for the wire.
// See ADR-010 for the model.
//
// All helpers fall back to 'UTC' if no tenant TZ is set (e.g. before
// /v1/settings has loaded). The fallback matches the DB default and
// keeps the dashboard usable during the boot window.

function tenantTZ(): string {
  return getTenantTimezone() || 'UTC'
}

// endOfDayInTZ returns UTC ISO 8601 of "yyyy-mm-dd at 23:59:59.999
// in tenant TZ". Used for civil-date expiry pickers ("expires
// May 5") so the operator-intended last-second-of-the-day matches
// regardless of who's viewing or where they are physically.
export function endOfDayInTZ(yyyymmdd: string, timezone?: string): string {
  const tz = timezone || tenantTZ()
  return fromZonedTime(`${yyyymmdd} 23:59:59.999`, tz).toISOString()
}

// startOfDayInTZ returns UTC ISO 8601 of "yyyy-mm-dd at 00:00:00.000
// in tenant TZ". Used for date-range filters ("from May 1") so the
// "from" boundary matches the operator's mental model of "from the
// start of that day".
export function startOfDayInTZ(yyyymmdd: string, timezone?: string): string {
  const tz = timezone || tenantTZ()
  return fromZonedTime(`${yyyymmdd} 00:00:00.000`, tz).toISOString()
}

// civilDateAgo returns the yyyy-mm-dd civil date N days or months before
// today in tenant TZ. Unlike addDaysInTZ it returns the CIVIL date, not an
// instant: date-range filter state is stored as civil dates and converted at
// query time by startOfDayInTZ/endOfDayInTZ, so a preset window has to be
// expressed in the same shape an operator's own picks take.
//
// Month arithmetic is calendar-based, not 30-day: "12 months" from Mar 31
// lands on the previous Mar 31, and JS Date normalizes the overflow days of
// short months (Mar 31 minus 1 month = Mar 3, not Feb 31) — acceptable for a
// coarse filter preset, and the same idiom the helpers above use.
export function civilDateAgo(amount: number, unit: 'day' | 'month', timezone?: string, now: Date = new Date()): string {
  const tz = timezone || tenantTZ()
  const todayStr = formatInTimeZone(now, tz, 'yyyy-MM-dd')
  const [y, m, d] = todayStr.split('-').map(Number)
  const target = unit === 'day' ? new Date(y, m - 1, d - amount) : new Date(y, m - 1 - amount, d)
  return `${target.getFullYear()}-${String(target.getMonth() + 1).padStart(2, '0')}-${String(target.getDate()).padStart(2, '0')}`
}

// addDaysInTZ rolls forward N days from today in tenant TZ and
// returns end-of-day-in-tenant-TZ as UTC ISO. The day-arithmetic
// uses a browser-local Date with explicit y/m/d construction (TZ-
// safe for day-grade math), then re-grounds in tenant TZ.
export function addDaysInTZ(days: number, timezone?: string): string {
  const tz = timezone || tenantTZ()
  const todayStr = formatInTimeZone(new Date(), tz, 'yyyy-MM-dd')
  const [y, m, d] = todayStr.split('-').map(Number)
  const target = new Date(y, m - 1, d + days)
  const targetStr = `${target.getFullYear()}-${String(target.getMonth() + 1).padStart(2, '0')}-${String(target.getDate()).padStart(2, '0')}`
  return endOfDayInTZ(targetStr, tz)
}

// addCalendarMonthInTZ steps an instant forward one CALENDAR month on the
// wall clock of `timezone`, clamping the day to the target month's last day
// (Jan 31 → Feb 28) and keeping the wall time. It mirrors the engine's
// domain.advanceAnchored month step (ADR-055 clamp, computed in the tenant
// billing TZ per ADR-058), so a "+1mo" preset lands on the cycle close the
// engine will compute. The month math runs on the zone's civil fields, never
// on the browser's local Date fields: a browser in another zone would
// otherwise step the wrong civil day (Oct 1 00:00Z read in Los Angeles is
// Sep 30, so it stepped to Oct 31 and missed the Nov 1 close). A wall time
// that falls in a DST gap resolves the way fromZonedTime resolves it.
export function addCalendarMonthInTZ(instant: Date, timezone: string): Date {
  const [y, m, d] = formatInTimeZone(instant, timezone, 'yyyy-MM-dd').split('-').map(Number)
  const wall = formatInTimeZone(instant, timezone, 'HH:mm:ss.SSS')
  // Month index m is 1-based, so m is already "next month" for Date.UTC; the
  // UTC constructor normalizes December → January of the next year.
  const next = new Date(Date.UTC(y, m, 1))
  const ny = next.getUTCFullYear()
  const nm = next.getUTCMonth() + 1
  const lastDay = new Date(Date.UTC(ny, nm, 0)).getUTCDate()
  const day = Math.min(d, lastDay)
  const civil = `${ny}-${String(nm).padStart(2, '0')}-${String(day).padStart(2, '0')}`
  return fromZonedTime(`${civil}T${wall}`, timezone)
}

// formatDateInTZ returns a human-readable date+time string in tenant
// TZ with zone abbreviation. Used by the "Key will expire on..."
// hint and similar copy that needs explicit TZ context inline.
export function formatExpiryHintInTZ(iso: string, timezone?: string): string {
  const tz = timezone || tenantTZ()
  return formatInTimeZone(new Date(iso), tz, "MMMM d, yyyy 'at' h:mm a (zzz)")
}

// formatInvoiceDate renders an instant on an invoice document (issued, due,
// paid, voided) as "MMM d, yyyy" in the invoice's own billing timezone, the
// zone the org had when the invoice was issued (ADR-077). The PDF and the
// hosted invoice page print these dates in that zone, so the dashboard must
// too: after the org re-zones, the live tenant zone can name a different day
// (due 2027-07-10T18:30Z is Jul 11 in Asia/Kolkata, Jul 10 in New York).
// An empty billing timezone (legacy or ad-hoc invoices) falls back to the
// tenant zone. `billingTimezone` is required so a caller must choose it.
export function formatInvoiceDate(iso: string, billingTimezone: string | null | undefined): string {
  const tz = billingTimezone || tenantTZ()
  return formatInTimeZone(new Date(iso), tz, 'MMM d, yyyy')
}

// formatYMD returns a yyyy-MM-dd string from a Date object — used
// when we need a date-only string for picker round-trips. Just a
// thin wrapper over date-fns format() so callers don't import
// date-fns directly for one-liners.
export function formatYMD(d: Date): string {
  return format(d, 'yyyy-MM-dd')
}

// formatYMDInTZ returns the yyyy-MM-dd date prefix of a UTC ISO
// timestamp re-projected into tenant TZ. Used by client-side list
// filters that compare a row's date against a picked date string —
// without this, a row with created_at = May 4 22:00 UTC (= May 5
// IST) would compare as "May 4" against an operator's "from May 5"
// pick and get filtered out, contradicting the dashboard's tenant-
// TZ display.
export function formatYMDInTZ(iso: string, timezone?: string): string {
  const tz = timezone || tenantTZ()
  return formatInTimeZone(new Date(iso), tz, 'yyyy-MM-dd')
}

// isAfterCivilToday reports whether a picked yyyy-mm-dd is a later civil day
// than "today" in tenant TZ, where today is the civil date of `nowMs` (the
// entity's effective now, so a test clock is honoured). Use it to validate a
// date that endOfDayInTZ will resolve in the same zone. Taking today from
// toISOString() (the UTC date) instead made a New York evening count as
// tomorrow, so tomorrow's date was refused as "not in the future".
export function isAfterCivilToday(yyyymmdd: string, nowMs: number, timezone?: string): boolean {
  return yyyymmdd > formatYMDInTZ(new Date(nowMs).toISOString(), timezone)
}

// calendarDaysUntil counts CALENDAR days from the anchor's civil date to the
// civil date of `iso`, both read in tenant TZ: 0 = same day, 1 = tomorrow,
// -1 = yesterday. Use it where the UI prints a date beside the count, as the
// invoice due badge does next to "Due Oct 16". An elapsed-time count
// (daysUntil, ceil over 24h) disagreed with that date because due_at carries
// the issue hour: due Oct 16 20:16 read "Due in 1d" on the morning of Oct 16
// and "Due today" until 20:16 the next day. NaN for an unparseable iso.
export function calendarDaysUntil(iso: string, now: EffectiveNow, timezone?: string): number {
  const t = new Date(iso)
  if (Number.isNaN(t.getTime())) return NaN
  const tz = timezone || tenantTZ()
  const dayNumber = (d: Date) => {
    const [y, m, day] = formatInTimeZone(d, tz, 'yyyy-MM-dd').split('-').map(Number)
    return Date.UTC(y, m - 1, day) / 86_400_000
  }
  return dayNumber(t) - dayNumber(new Date(now))
}

// civilDateFrom returns the yyyy-mm-dd civil date `days` after the civil date
// of `nowMs` in tenant TZ. Unlike civilDateAgo it takes the anchor as an
// argument, so a clock-pinned entity passes its simulated now. Use it for a
// picker floor whose date is later resolved with startOfDayInTZ in the same
// zone: the pause dialog's earliest resume date is civilDateFrom(now, 1),
// because start of today is already past and the server refuses it.
export function civilDateFrom(nowMs: number, days: number, timezone?: string): string {
  const tz = timezone || tenantTZ()
  const [y, m, d] = formatInTimeZone(new Date(nowMs), tz, 'yyyy-MM-dd').split('-').map(Number)
  const target = new Date(y, m - 1, d + days)
  return formatYMD(target)
}

// ---------------------------------------------------------------------------
// Inclusive billing-period display (ADR-058).
//
// Billing periods are stored HALF-OPEN [start, end): `end` is the EXCLUSIVE
// boundary where the next period begins (Jun 1 → Jul 1 = "covers June"). Every
// billing engine (Stripe, Zuora, Recurly, Chargebee, Lago) DISPLAYS a period by
// its INCLUSIVE last covered day instead — "Jun 1 – Jun 30", not "Jun 1 – Jul 1"
// — so the operator never has to mentally subtract a day.
//
// These two helpers are the client-side mirror of the Go functions
// domain.InclusiveDisplayEnd / domain.FormatInclusivePeriod (the canonical
// spec — keep them byte-for-byte identical). The CANONICAL billing-period range
// is authored by the backend and rendered verbatim — invoices via
// `billing_period_display` and subscriptions via `current_billing_period_display`
// (both computed by FormatInclusivePeriod in the org billing TZ). Prefer those
// authored strings over re-deriving from the half-open start/end, so the Go and
// TS inclusive-day logic can't drift.
//
// What remains for these helpers: auxiliary period/date labels the backend does
// not author — a trial range, a single inclusive period-end date, a line-item's
// "Covers …" span. Anchor them in the OWNING entity's billing timezone:
//   - INVOICE-anchored dates → pass `invoice.billing_timezone` (the org TZ
//     denormalized at issue, ADR-077 — stays fixed if the org later re-zones).
//   - SUBSCRIPTION-anchored dates → pass undefined; a sub bills in the one org
//     timezone, which IS the live tenant setting, so the tenant-TZ fallback is
//     the correct anchor (no per-sub snapshot exists to diverge from it, ADR-077
//     supersedes ADR-074). Display only — the wire stays half-open.
//
// The conversion is a CALENDAR step, never a 24h instant subtraction: snap the
// exclusive boundary to its civil date in the given TZ, then step back one
// calendar day. A 24h subtraction lands on the wrong civil date across a DST
// boundary or a non-midnight end — the same off-by-one class ADR-058 fixed in
// the engine.

// inclusiveEndYMD returns [year, month, day] of the last calendar day fully
// covered by a half-open period whose exclusive end is `endISO`, in tenant TZ.
function inclusiveEndYMD(endISO: string, tz: string): [number, number, number] {
  // The boundary's civil date in tenant TZ (e.g. 2028-07-01).
  const civil = formatInTimeZone(new Date(endISO), tz, 'yyyy-MM-dd')
  const [y, m, d] = civil.split('-').map(Number)
  // Step back ONE calendar day. JS Date normalizes day 0 → last day of the
  // previous month and Jan 0 → Dec 31 of the prior year, so month/year rollover
  // and leap-Februarys fall out correctly. Explicit y/m/d construction keeps
  // this browser-TZ-safe (same idiom as addDaysInTZ above).
  const back = new Date(y, m - 1, d - 1)
  return [back.getFullYear(), back.getMonth() + 1, back.getDate()]
}

// usageFilterEndYMD turns a half-open period [start, end) into the INCLUSIVE
// last day of a yyyy-mm-dd date-range filter: the drill-down from an invoice
// line to the usage events it billed. The link must show exactly what the
// invoice billed, and date-range filters include their whole end day.
//
//   - end at civil midnight in `timezone` (a normal cycle, Jun 1 → Jul 1): the
//     end day belongs to the NEXT period, so step back one calendar day
//     (→ Jun 30). Passing the boundary through would add a day of usage this
//     invoice did not bill.
//   - end mid-day (a cancel invoice ends at the cancel instant, Oct 16 15:00):
//     usage up to that instant on the end day WAS billed, so the end day is
//     kept (→ Oct 16). Stepping back would drop it, and a same-day cancel
//     would give a "to" before its "from".
//   - never before the start's civil day, so the range can't invert.
//
// A filter is day-grained, so a mid-day end can't be matched exactly: the
// link then also covers the rest of the end day, which is the smaller error.
export function usageFilterEndYMD(startISO: string, endISO: string, timezone?: string): string {
  const tz = timezone || tenantTZ()
  const end = new Date(endISO)
  let ymd: string
  if (formatInTimeZone(end, tz, 'HH:mm:ss.SSS') === '00:00:00.000') {
    const [y, m, d] = inclusiveEndYMD(endISO, tz)
    ymd = `${y}-${String(m).padStart(2, '0')}-${String(d).padStart(2, '0')}`
  } else {
    ymd = formatInTimeZone(end, tz, 'yyyy-MM-dd')
  }
  const startYMD = formatInTimeZone(new Date(startISO), tz, 'yyyy-MM-dd')
  // yyyy-mm-dd strings compare correctly as text.
  return ymd < startYMD ? startYMD : ymd
}

// formatCivilDate renders the INCLUSIVE last covered day of a half-open
// period_end as "MMM d, yyyy" ("Jun 30, 2028"). Use for a single period-boundary
// label that means "last day covered" (e.g. a cycle's end). NOT for event dates
// ("Renews", "Cancels", "Next billing") — those fire on the exclusive boundary
// instant and must keep formatDate(). Empty input → "". Mirrors Go
// domain.InclusiveDisplayEnd + the "Jan 2, 2006" layout.
//
// `timezone` is REQUIRED (ADR-076): a civil-day render must name its zone —
// `invoice.billing_timezone` for an invoice-anchored date, or undefined for a
// subscription-anchored one (→ live tenant/org TZ, the sub's actual anchor,
// ADR-077). The type forbids OMITTING the argument so a caller can't render a
// civil day without consciously choosing its zone (the FE bug class).
export function formatCivilDate(endISO: string | null | undefined, timezone: string | undefined): string {
  if (!endISO) return ''
  const tz = timezone || tenantTZ()
  const [y, m, d] = inclusiveEndYMD(endISO, tz)
  return format(new Date(y, m - 1, d), 'MMM d, yyyy')
}

// formatCivilPeriod renders the human period range "<start> – <inclusiveEnd>"
// (en-dash) in tenant TZ — the exact string the invoice's backend
// billing_period_display shows, so a period reads identically wherever it
// appears. Returns "" when the period is empty (start == end). Clamps the
// inclusive end to >= the start's civil day so a sub-day stub never inverts.
// Mirrors Go domain.FormatInclusivePeriod.
// `timezone` is REQUIRED (ADR-076) — see formatCivilDate. Pass
// `invoice.billing_timezone` for an invoice-anchored period, or undefined for a
// subscription-anchored one (→ live tenant/org TZ); the argument can't be omitted.
export function formatCivilPeriod(
  startISO: string | null | undefined,
  endISO: string | null | undefined,
  timezone: string | undefined,
): string {
  if (!startISO || !endISO) return ''
  if (new Date(startISO).getTime() === new Date(endISO).getTime()) return ''
  const tz = timezone || tenantTZ()
  const startStr = formatInTimeZone(new Date(startISO), tz, 'MMM d, yyyy')
  // Start's civil day in tenant TZ — the clamp floor for the inclusive end.
  const [sy, sm, sd] = formatInTimeZone(new Date(startISO), tz, 'yyyy-MM-dd')
    .split('-')
    .map(Number)
  const startDate = new Date(sy, sm - 1, sd)
  const [ey, em, ed] = inclusiveEndYMD(endISO, tz)
  let endDate = new Date(ey, em - 1, ed)
  if (endDate.getTime() < startDate.getTime()) endDate = startDate
  return `${startStr} – ${format(endDate, 'MMM d, yyyy')}`
}
