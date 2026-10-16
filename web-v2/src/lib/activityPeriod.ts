import { formatCivilPeriod } from './dates'

// The customer Activity card's header line for the window it shows.
//
// The server answers with a half-open window [from, to). On the Cycle preset
// (source "current_billing_cycle") that window is the subscription's current
// billing period, and `to` is the exclusive boundary where the next period
// starts. Printed raw it read "Oct 1, 2026 → Nov 1, 2026" right under the
// Upcoming card's "Oct 1, 2026 – Oct 31, 2026" for the same period. A cycle is
// therefore shown as its inclusive civil period, the same string the server
// authors as current_billing_period_display (formatCivilPeriod mirrors it).
//
// Explicit windows (7d, 30d, 90d) end one millisecond past "now", not at a
// day boundary, so stepping back a day would hide today. They keep their
// plain from → to dates: this returns null and the caller renders those.
//
// `timezone` undefined → the tenant (org) timezone, the subscription's anchor.
export function activityCyclePeriodLabel(
  period: { from: string; to: string; source: string },
  timezone: string | undefined,
): string | null {
  if (period.source !== 'current_billing_cycle') return null
  return formatCivilPeriod(period.from, period.to, timezone) || null
}
