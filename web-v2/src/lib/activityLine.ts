// How the dashboard shows an activity line (ADR-128). The backend builds
// every word of a line; this file only formats money and times, turns a link
// into a route, maps the tone to a colour and groups rows by day. No event
// words live here. Plain data in, plain data out, so the unit tests can check
// it without rendering.
//
// Times use the account's timezone, passed in. With none set, they use the
// browser's zone, and the zone header names that zone instead.

import type { ActivityActor } from './gen/schemas/activityActor'
import type { ActivityLine } from './gen/schemas/activityLine'
import type { ActivityLink } from './gen/schemas/activityLink'
import type { ActivityMoney } from './gen/schemas/activityMoney'
import { formatCents } from './money'
import { wallClockNow } from './effectiveNow'

// The four tones, one meaning each (ADR-128 rule 7). The words carry the
// meaning; the dot only repeats it. Green is for money only.
export const TONES = {
  neutral: { dot: 'bg-muted-foreground', problem: 'text-muted-foreground' },
  success: { dot: 'bg-success', problem: 'text-success-foreground' },
  warning: { dot: 'bg-warning', problem: 'text-warning-foreground' },
  failure: { dot: 'bg-destructive', problem: 'text-destructive' },
} as const

export type Tone = keyof typeof TONES

// toneOf reads a line's tone. A tone this dashboard does not know reads as
// neutral, so a newer server never draws a row without a dot.
export function toneOf(tone: string | undefined): Tone {
  return tone && tone in TONES ? (tone as Tone) : 'neutral'
}

// money prints an amount in its own currency, never the workspace default.
export function money(m: ActivityMoney): string {
  return formatCents(m.cents, m.currency)
}

// actorText words who did it, as the backend's activity.ActorText does. It
// is the meta line's last part before the Recorded part.
export function actorText(actor: ActivityActor | undefined): string {
  if (!actor) return ''
  switch (actor.kind) {
    case 'person':
      return actor.name ? `by ${actor.name}` : ''
    case 'api_key':
      return actor.name ? `by API key ${actor.name}` : 'by API key'
    case 'customer':
      if (actor.via === 'payment_page') return 'Customer, on the payment page'
      if (actor.via === 'portal') return 'Customer, in the portal'
      return 'Customer'
    case 'automatic':
      return 'Automatic'
  }
  return ''
}

// linkRoute is the dashboard route for a link part, or null when there is
// none; the part then shows as plain text. Credit notes have no page of
// their own, so the link opens the credit-note list filtered to that number.
export function linkRoute(link: ActivityLink): string | null {
  switch (link.kind) {
    case 'invoice':
      return `/invoices/${encodeURIComponent(link.id)}`
    case 'subscription':
      return `/subscriptions/${encodeURIComponent(link.id)}`
    case 'credit_note':
      return link.label ? `/credit-notes?search=${encodeURIComponent(link.label)}` : null
  }
  return null
}

function parts(iso: string, tz: string | undefined, opts: Intl.DateTimeFormatOptions): Record<string, string> {
  const out: Record<string, string> = {}
  for (const p of new Intl.DateTimeFormat('en-US', { ...opts, ...(tz ? { timeZone: tz } : {}) }).formatToParts(new Date(iso))) {
    out[p.type] = p.value
  }
  return out
}

// dayKey is the calendar day an instant falls on in the zone, as yyyy-mm-dd.
export function dayKey(iso: string, tz: string | undefined): string {
  const p = parts(iso, tz, { year: 'numeric', month: '2-digit', day: '2-digit' })
  return `${p.year}-${p.month}-${p.day}`
}

// dayLabel is a day header: "Feb 1, 2027".
export function dayLabel(iso: string, tz: string | undefined): string {
  return new Date(iso).toLocaleDateString('en-US', {
    year: 'numeric', month: 'short', day: 'numeric',
    ...(tz ? { timeZone: tz } : {}),
  })
}

// timeOfDay is a row's time, under its day header: "10:00 AM".
export function timeOfDay(iso: string, tz: string | undefined): string {
  return new Date(iso).toLocaleTimeString('en-US', {
    hour: 'numeric', minute: '2-digit',
    ...(tz ? { timeZone: tz } : {}),
  })
}

// partTime is a time inside the meta line ("Trial now ends Nov 1, 2026,
// 9:00 AM"). The card header names the zone, so the part does not.
export function partTime(iso: string, tz: string | undefined): string {
  return new Date(iso).toLocaleString('en-US', {
    year: 'numeric', month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit',
    ...(tz ? { timeZone: tz } : {}),
  })
}

// recordedTime is the Recorded part's date and time: the real moment, which
// is on the other calendar, so it keeps its date. The year shows only when
// it is not the current year. The Recorded part is the real moment, so
// "current" is the wall clock's year, never a test clock's.
export function recordedTime(iso: string, tz: string | undefined, nowMs: number = wallClockNow()): string {
  const sameYear = dayKey(iso, tz).slice(0, 4) === dayKey(new Date(nowMs).toISOString(), tz).slice(0, 4)
  return new Date(iso).toLocaleString('en-US', {
    ...(sameYear ? {} : { year: 'numeric' }),
    month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit',
    ...(tz ? { timeZone: tz } : {}),
  })
}

// zoneName is the zone as the dashboard's formatDateTime names it:
// "GMT+5:30" for Asia/Kolkata, "EST" or "EDT" for America/New_York.
function zoneName(iso: string, tz: string | undefined): string {
  return parts(iso, tz, { timeZoneName: 'short' }).timeZoneName ?? ''
}

// zoneHeader names the zone the card's times are in, once per card: "Times
// in GMT+5:30". A zone with summer time can name two ("Times in EST / EDT")
// when the rows fall on both sides of a change.
export function zoneHeader(isos: readonly string[], tz: string | undefined): string {
  const names: string[] = []
  for (const iso of isos) {
    const n = zoneName(iso, tz)
    if (n && !names.includes(n)) names.push(n)
  }
  return names.length > 0 ? `Times in ${names.join(' / ')}` : ''
}

export interface ActivityDay {
  key: string
  label: string
  // The instant the header's <time> element carries: the day's first row.
  at: string
  lines: ActivityLine[]
}

// groupByDay puts the lines under day headers on the entity's calendar,
// keeping the server's order (timeline.Order). A day that comes back after
// another day gets a header again, so the order is never changed here.
export function groupByDay(lines: readonly ActivityLine[], tz: string | undefined): ActivityDay[] {
  const days: ActivityDay[] = []
  for (const line of lines) {
    const key = dayKey(line.occurred_at, tz)
    const last = days[days.length - 1]
    if (last && last.key === key) {
      last.lines.push(line)
    } else {
      days.push({ key, label: dayLabel(line.occurred_at, tz), at: line.occurred_at, lines: [line] })
    }
  }
  return days
}
