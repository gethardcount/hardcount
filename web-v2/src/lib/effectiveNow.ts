// The "now" every relative value on the dashboard is measured from: "2h ago",
// "due in 3 days", "day 12 of 30", a rolling 30-day window.
//
// A record on a test clock lives at the clock's time, which can be months
// away from real time, so measuring it against Date.now() draws wrong values.
// EffectiveNow is a branded number that only the two constructors below make,
// and every helper in this file requires one. A page therefore cannot measure
// from "now" without saying which now it means:
//   - for a record, take it from useNowFor or useNowResolver (hooks/useNow),
//     which answer with the record's clock time, real time, or undefined
//     while it is not known; draw nothing counted from now while it is
//     undefined;
//   - wallClockNow() marks a surface that is real time on purpose: the audit
//     log, the email and webhook outbox, API keys.
// The no-restricted-syntax rule in eslint.config.js bans a bare Date.now() or
// new Date() in display code, so nothing goes around the constructors.

declare const EFFECTIVE_NOW: unique symbol

// EffectiveNow is epoch milliseconds. Arithmetic works, because it is a
// number; a bare number does not assign to it.
export type EffectiveNow = number & { readonly [EFFECTIVE_NOW]: true }

// clockTime is a test clock's time: pass the clock's frozen_time.
export function clockTime(frozenISO: string): EffectiveNow {
  return new Date(frozenISO).getTime() as EffectiveNow
}

// wallClockNow is real time, for a surface that does not follow any clock.
export function wallClockNow(): EffectiveNow {
  // eslint-disable-next-line no-restricted-syntax -- this IS the wall-clock seam
  return Date.now() as EffectiveNow
}

// ---------------------------------------------------------------------------
// Primitives — the arithmetic every formatter shares. Site-specific wording
// (Dunning's "yesterday", CostDashboard's weeks/months) builds on these so it
// keeps its copy while still requiring an anchor.
// ---------------------------------------------------------------------------

// sinceMs is `now - iso` (positive when iso is in the past). NaN if iso is
// unparseable — callers that render user copy should guard.
export function sinceMs(iso: string, now: EffectiveNow): number {
  return now - new Date(iso).getTime()
}

// untilMs is `iso - now` (positive when iso is in the future).
export function untilMs(iso: string, now: EffectiveNow): number {
  return new Date(iso).getTime() - now
}

// ---------------------------------------------------------------------------
// Canonical helpers
// ---------------------------------------------------------------------------

// timeAgo renders "just now / Nm ago / Nh ago / Nd ago" (day-granularity tail).
// Canonical replacement for the byte-identical api.ts formatRelativeTime and
// InvoiceAttention formatRelative. NaN-safe (→ '') and clamped at 0 so a
// slightly-future timestamp reads "just now" rather than a negative age.
export function timeAgo(iso: string, now: EffectiveNow): string {
  const t = new Date(iso).getTime()
  if (Number.isNaN(t)) return ''
  const sec = Math.max(0, Math.floor((now - t) / 1000))
  if (sec < 60) return 'just now'
  const min = Math.floor(sec / 60)
  if (min < 60) return `${min}m ago`
  const hr = Math.floor(min / 60)
  if (hr < 24) return `${hr}h ago`
  const days = Math.floor(hr / 24)
  return `${days}d ago`
}

// timeUntil is timeAgo's future-tense twin for timestamps that are
// EXPECTED to be ahead of now (retry schedules, resumes). Rendering a
// future instant through timeAgo clamps to "just now" — a failed
// webhook delivery with next_retry_at 4h out read "next retry just
// now" for the whole 4 hours. Past inputs (dispatcher lag) collapse
// to "imminently" rather than lying with an age.
export function timeUntil(iso: string, now: EffectiveNow): string {
  const t = new Date(iso).getTime()
  if (Number.isNaN(t)) return ''
  const sec = Math.floor((t - now) / 1000)
  if (sec <= 0) return 'imminently'
  if (sec < 60) return 'in under a minute'
  const min = Math.floor(sec / 60)
  if (min < 60) return `in ${min}m`
  const hr = Math.floor(min / 60)
  if (hr < 24) return `in ${hr}h`
  const days = Math.floor(hr / 24)
  return `in ${days}d`
}

// daysUntil is the ceil-day count of elapsed time used by ExpiryBadge: >0
// future, 0 today, <0 past. Matches the prior `Math.ceil((t - ref) / 86_400_000)`.
// DueBadge counts calendar days instead (dates.ts calendarDaysUntil), because
// it sits next to a printed due date.
export function daysUntil(iso: string, now: EffectiveNow): number {
  return Math.ceil((new Date(iso).getTime() - now) / 86_400_000)
}

// isOlderThan gates "since X ago" badges: true when iso predates `now` by more
// than thresholdMs. NaN iso → false (don't gate on garbage).
export function isOlderThan(iso: string, thresholdMs: number, now: EffectiveNow): boolean {
  const t = new Date(iso).getTime()
  if (Number.isNaN(t)) return false
  return now - t > thresholdMs
}

// cycleProgress reports "Day N of M · P%" for a billing period, measured
// against the anchor. Clamped so a mid-advance cycle can't exceed 100%.
export function cycleProgress(
  start: string,
  end: string,
  now: EffectiveNow,
): { daysIn: number; totalDays: number; pct: number } {
  const startMs = new Date(start).getTime()
  const endMs = new Date(end).getTime()
  const totalDays = Math.max(1, Math.round((endMs - startMs) / 86_400_000))
  const daysIn = Math.max(0, Math.min(totalDays, Math.round((now - startMs) / 86_400_000)))
  const pct = totalDays > 0 ? Math.min(100, (daysIn / totalDays) * 100) : 0
  return { daysIn, totalDays, pct }
}

// rollingWindow returns the {from, to} ISO bounds of a trailing N-day window
// that INCLUDES the anchor — a "last 7/30/90d" usage query. For a clock-pinned
// customer this frames the simulated window, not a wall-clock one that
// predates the simulated events.
//
// `to` is one millisecond past the anchor because the usage and margin queries
// treat `to` as exclusive (`timestamp < to`), and an event sent without a
// timestamp to a clock-pinned customer is stamped at exactly the clock's
// frozen_time (usage.Service.Ingest). With `to` equal to the anchor, that
// event, the newest usage the customer has, fell outside every rolling window
// while the current-cycle view showed it.
export function rollingWindow(days: number, now: EffectiveNow): { from: string; to: string } {
  return {
    from: new Date(now - days * 86_400_000).toISOString(),
    to: new Date(now + 1).toISOString(),
  }
}
