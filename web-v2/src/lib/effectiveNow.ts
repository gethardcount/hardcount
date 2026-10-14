// effectiveNow — the one way to obtain a "now" for relative-time UI.
//
// THE PROBLEM THIS CLOSES
// Test clocks let an operator advance simulated time; a clock-pinned entity's
// timestamps (billing-period bounds, due/expiry dates, created_at) are stamped
// in simulation time and can sit far in the browser's future or past. Any UI
// that renders a value RELATIVE to "now" — "X ago", "in N days", cycle %, "Day
// N of M", a rolling 30d window — must read that "now" from the entity's clock,
// or it lies. The #410/#411/#413 audit fixed ~59 such sites by threading an
// optional `nowISO`, but every helper still ended in `?? Date.now()`: forget to
// pass the anchor and the surface silently reverts to wall-clock, wrong only
// once a clock is advanced. That silent fallback is the drift class.
//
// THE MECHANISM
// `EffectiveNow` is a branded epoch-ms value. A plain number/Date/string is NOT
// assignable to it — the ONLY ways to make one are the two constructors below,
// and every relative-time helper REQUIRES one (no `?? Date.now()` fallback
// anywhere). So "did you handle the test clock?" stops being a discipline you
// can forget and becomes a compile error: you must call either
//   • effectiveNow(frozenISO)  — anchor an ENTITY surface on its resolved clock
//                                time (frozen_time when pinned, wall time when
//                                not — an unpinned entity genuinely has wall
//                                time as its effective now), or
//   • wallClockNow()           — an EXPLICIT, greppable "this surface is
//                                deliberately real-time" (forensic/egress
//                                layers — audit log, email/webhook outbox — and
//                                entities that can never be clock-pinned:
//                                API keys, webhook endpoints).
//
// This is the ADR-086 Phase-4 endpoint: a required-anchor helper. It sits over
// the existing client-side frozen-time resolution (useClockFrozenMap); if the
// backend later ships `effective_now` per row (Pillar 1, the dunning-run
// pattern), effectiveNow() takes that value with no site changes.
//
// See useEffectiveNowResolver (hooks/useClockFrozenMap) for the ergonomic
// list/feed path, and the ESLint no-restricted-syntax rule that bans raw
// Date.now()/new Date() in display code so nothing bypasses these constructors.

declare const EFFECTIVE_NOW: unique symbol

// EffectiveNow is epoch milliseconds, branded so it can only originate from a
// constructor below. Arithmetic works (it is a number subtype); construction
// does not (a bare number won't assign).
export type EffectiveNow = number & { readonly [EFFECTIVE_NOW]: true }

// effectiveNow builds the anchor for an entity-bound surface from its resolved
// clock time. Pass the owning test clock's frozen_time (ISO) when the entity is
// clock-pinned; pass null/undefined when it isn't (→ wall time). Pair with
// clockNow()/useClockFrozenMap or a single-clock fetch to produce the argument.
export function effectiveNow(frozenISO?: string | null): EffectiveNow {
  // eslint-disable-next-line no-restricted-syntax -- this IS the wall-clock seam
  return (frozenISO ? new Date(frozenISO).getTime() : Date.now()) as EffectiveNow
}

// wallClockNow is the explicit wall-clock anchor for surfaces that are meant to
// be real-time regardless of any clock. Using it is a documented decision, not
// an accidental Date.now() — reviewers can grep for it.
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

// daysUntil is the ceil-day count used by DueBadge/ExpiryBadge: >0 future,
// 0 today, <0 past. Matches the prior `Math.ceil((t - ref) / 86_400_000)`.
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
