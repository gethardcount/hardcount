import type { HeldUsageGroup } from './gen/schemas/heldUsageGroup'
import type { HeldUsageReleaseResult } from './gen/schemas/heldUsageReleaseResult'

// Held usage (#1044): LiteLLM rows whose customer or meter did not exist yet.
// They are kept, not billed, until the operator releases or discards them.
// The server groups by (external id, meter, reason); the page acts per
// external id, because Release and Discard take one.

export interface HeldCustomer {
  externalId: string
  // The customer whose external ID matches now, or '' when none does yet.
  customerId: string
  rows: number
  missingMeters: string[]
  firstEventAt: string
  lastEventAt: string
}

export function groupHeldByCustomer(groups: HeldUsageGroup[]): HeldCustomer[] {
  const out = new Map<string, HeldCustomer>()
  for (const g of groups) {
    let c = out.get(g.external_customer_id)
    if (!c) {
      c = {
        externalId: g.external_customer_id,
        customerId: g.customer_id,
        rows: 0,
        missingMeters: [],
        firstEventAt: g.first_event_at,
        lastEventAt: g.last_event_at,
      }
      out.set(g.external_customer_id, c)
    }
    c.rows += g.rows
    if (!g.meter_exists && !c.missingMeters.includes(g.meter_key)) c.missingMeters.push(g.meter_key)
    if (g.first_event_at < c.firstEventAt) c.firstEventAt = g.first_event_at
    if (g.last_event_at > c.lastEventAt) c.lastEventAt = g.last_event_at
  }
  return [...out.values()]
}

function rowsWord(n: number): string {
  return `${n.toLocaleString('en-US')} usage ${n === 1 ? 'row' : 'rows'}`
}

// heldLine is the notice's one line per external ID: what is held and why.
export function heldLine(c: HeldCustomer): string {
  if (!c.customerId) return `${rowsWord(c.rows)} held: customer '${c.externalId}' does not exist yet`
  if (c.missingMeters.length > 0) {
    const meters = c.missingMeters.map(m => `'${m}'`).join(', ')
    return `${rowsWord(c.rows)} held for customer '${c.externalId}': meter ${meters} does not exist yet`
  }
  return `${rowsWord(c.rows)} held for customer '${c.externalId}'`
}

// heldNextStep says what to do about one line.
export function heldNextStep(c: HeldCustomer): string {
  if (!c.customerId) {
    return `Create a customer with external ID '${c.externalId}', then release them. If the ID is wrong, discard them.`
  }
  if (c.missingMeters.length > 0) {
    return `Create the missing meter, then release them.`
  }
  return 'The customer exists now. Release them to record the usage.'
}

// canRelease: the customer exists. Rows whose meter is still missing stay
// held, so a release is still useful when only some meters exist; the
// server says how many stayed.
export function canRelease(c: HeldCustomer, groups: HeldUsageGroup[]): boolean {
  if (!c.customerId) return false
  return groups.some(g => g.external_customer_id === c.externalId && g.meter_exists)
}

// heldUnlisted says how much the notice does not show: the server lists the
// customers held most recently (200), and its totals count all of them.
// Returns '' when everything is listed.
export function heldUnlisted(listed: HeldCustomer[], totalCustomers: number, totalRows: number): string {
  const hidden = totalCustomers - listed.length
  if (hidden <= 0) return ''
  const shownRows = listed.reduce((n, c) => n + c.rows, 0)
  const hiddenRows = Math.max(totalRows - shownRows, 0)
  return `Showing the ${listed.length.toLocaleString('en-US')} customers held most recently. ${rowsWord(hiddenRows)} for ${hidden.toLocaleString('en-US')} more ${hidden === 1 ? 'customer' : 'customers'} ${hiddenRows === 1 ? 'is' : 'are'} held too; release or discard these to see them.`
}

// releaseSummary is the toast after a release. It says plainly which rows a
// subscription invoice will count, because rows from before the customer
// existed usually fall before any billing period.
export function releaseSummary(r: HeldUsageReleaseResult): { title: string; description: string } {
  const recorded = r.released + r.deduplicated
  const title = recorded === 0
    ? 'Nothing released'
    : `Released ${rowsWord(recorded)}`
  const parts: string[] = []
  if (r.in_open_period > 0) parts.push(`${r.in_open_period.toLocaleString('en-US')} fall in the current billing period and will be invoiced.`)
  if (r.not_on_plan > 0) {
    parts.push(`${r.not_on_plan.toLocaleString('en-US')} fall in the current billing period, but the subscription's plan has no price for their meter, so no invoice charges them.`)
  }
  if (r.outside_open_period > 0) {
    parts.push(`${r.outside_open_period.toLocaleString('en-US')} fall outside the current billing period, so no subscription invoice includes them unless that period's invoice is not built yet. Bill them with a one-off invoice if needed.`)
  }
  if (r.deduplicated > 0) parts.push(`${r.deduplicated.toLocaleString('en-US')} were already recorded and were not added again.`)
  if (r.more_to_release > 0) parts.push(`${rowsWord(r.more_to_release)} more are ready. One release takes at most 5,000 rows; release again to record them.`)
  if (r.still_held > 0) parts.push(`${rowsWord(r.still_held)} still held: a meter is missing or the time is after the customer's current time.`)
  return { title, description: parts.join(' ') }
}
