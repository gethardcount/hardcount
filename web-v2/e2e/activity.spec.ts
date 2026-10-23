import path from 'node:path'
import { test, expect, type Locator, type Page } from '@playwright/test'
import { api, uniq } from './fixtures/api'
import { createCustomer, invoicesFor, waitForClockReady, type Customer, type Plan, type Subscription, type TestClock } from './fixtures/seed'

// Screenshots of the activity cards, for a person to compare before and after
// a change to activity lines (ADR-128). They are not a pass/fail check: the
// spec checks only that each card renders.
//
// Each story runs twice, for a customer on the real clock and for one on a
// test clock frozen at FROZEN, so the test-clock dates are the same on every
// run. The operator's steps are the same on both. Steps that need days to pass
// (payment retries, a trial running out) happen only on the test clock: a real
// customer has not reached those days yet.
//
// Shots go to activity-shots/<surface>-<real|test>-<width>.png. CI uploads
// them as the activity-shots artifact (README.md says how to fetch them).

const FROZEN = '2027-02-01T04:30:00Z'
const DAY = 86_400_000
const WIDTHS = [1440, 390]
const SHOTS_DIR = path.join(import.meta.dirname, '..', 'activity-shots')

type Clock = 'real' | 'test'
interface Person extends Customer { email: string }
interface Sub extends Subscription { trial_end_at?: string }
interface Invoice { id: string; invoice_number: string }
interface DunningRun { state: string }

// A story makes its data, then opens three pages and takes two shots on each.
test.describe.configure({ timeout: 120_000 })

// The call the dashboard makes, on the signed-in person's session, so the
// activity row names that person. api() uses the secret key, so its rows name
// the API key.
async function asPerson<T>(page: Page, method: string, apiPath: string, body?: unknown): Promise<T> {
  const res = await page.request.fetch(`/v1${apiPath}`, { method, data: body })
  const text = await res.text()
  if (!res.ok()) throw new Error(`${method} /v1${apiPath} as the signed-in person returned ${res.status()}: ${text}`)
  return (text ? JSON.parse(text) : undefined) as T
}

function iso(ms: number): string {
  return new Date(ms).toISOString().replace(/\.\d+Z$/, 'Z')
}

// The customer for one run of a story: on the real clock, or on a new test
// clock frozen at FROZEN.
async function customerOn(clock: Clock): Promise<{ customer: Person; testClock?: TestClock }> {
  if (clock === 'real') return { customer: (await createCustomer()) as Person }
  const testClock = await api<TestClock>('POST', '/test-clocks', { name: `E2E Activity ${uniq()}`, frozen_time: FROZEN })
  return { customer: (await createCustomer(testClock.id)) as Person, testClock }
}

async function advanceTo(testClock: TestClock, ms: number) {
  await api('POST', `/test-clocks/${testClock.id}/advance`, { frozen_time: iso(ms) })
  await waitForClockReady(testClock.id)
}

// A monthly USD plan whose fee is billed at the start of each period, so a
// subscription is invoiced as soon as it starts, or as soon as its trial ends.
function planBilledInAdvance(): Promise<Plan> {
  const id = uniq()
  return api<Plan>('POST', '/plans', {
    code: `e2e-act-${id}`,
    name: `E2E Activity ${id}`,
    currency: 'USD',
    billing_interval: 'monthly',
    base_amount_cents: 4900,
    base_bill_timing: 'in_advance',
  })
}

// Gives the customer a dunning policy of its own: three retries, on days 3, 7
// and 14, then collection is paused. Other specs change the workspace's
// default policy while they run, so the default would give a different
// story on different runs. The policy is created turned off and then turned
// on, because a new policy created turned on takes the default from the
// one the account was created with (ADR-123).
async function ownRetryPolicy(customerId: string) {
  const policy = {
    name: `E2E Activity retries ${uniq()}`,
    max_retry_attempts: 3,
    grace_period_days: 3,
    retry_schedule: ['96h', '168h'],
    final_subscription_action: 'pause',
    final_invoice_action: 'none',
  }
  const created = await api<{ id: string }>('POST', '/dunning/policies', { ...policy, enabled: false })
  await api('PATCH', `/dunning/policies/${created.id}`, { ...policy, enabled: true })
  await api('PATCH', `/customers/${customerId}`, { dunning_policy_id: created.id })
}

function subscribe(customerId: string, planId: string, extra: Record<string, unknown> = {}): Promise<Sub> {
  const id = uniq()
  return api<Sub>('POST', '/subscriptions', {
    code: `e2e-act-${id}`,
    display_name: `E2E Activity ${id}`,
    customer_id: customerId,
    items: [{ plan_id: planId, quantity: 1 }],
    billing_time: 'anniversary',
    ...extra,
  })
}

// Emails are sent by a background worker. Waits until none is still queued,
// so the shots show what happened to each one.
async function waitForEmailsSent(customerId: string) {
  await expect.poll(async () => {
    const res = await api<{ sent_emails: { status: string }[] }>('GET', `/customers/${customerId}/sent-emails`)
    return res.sent_emails.filter(e => e.status === 'pending').length
  }, { message: 'no email is still queued', timeout: 60_000 }).toBe(0)
}

function card(page: Page, title: string | RegExp): Locator {
  return page.getByRole('main').locator('[data-slot="card"]').filter({ has: page.getByText(title, { exact: true }) })
}

// Takes one shot at each width. The dashboard scrolls inside <main>, so a
// card below the fold would be cut off; the viewport grows to the page's
// height first.
async function atEachWidth(page: Page, name: string, shoot: (file: string) => Promise<unknown>) {
  for (const width of WIDTHS) {
    let height = 900
    for (let i = 0; i < 3; i++) {
      await page.setViewportSize({ width, height })
      const needed = await page.getByRole('main').evaluate(el => el.scrollHeight)
      if (needed <= height) break
      height = needed
    }
    await shoot(path.join(SHOTS_DIR, `${name}-${width}.png`))
  }
}

async function shootCard(page: Page, target: Locator, name: string) {
  await expect(target).toBeVisible()
  await atEachWidth(page, name, file => target.screenshot({ path: file, animations: 'disabled' }))
}

const STATE_FILTER: Record<string, string> = { active: 'Active', escalated: 'Escalated', resolved: 'Resolved' }
const RUNS_PAGE_SIZE = 25

// The Dunning page has no link to a single run and reads no URL parameters,
// so the run is found as a person finds it: filter by its state, then page
// through. The shot is the run's row and its open recovery timeline.
async function shootDunningRun(page: Page, invoice: Invoice, name: string) {
  const [run] = (await api<{ data: DunningRun[] }>('GET', `/dunning/runs?invoice_id=${invoice.id}`)).data
  expect(run, `invoice ${invoice.invoice_number} has a dunning run`).toBeTruthy()
  await page.goto('/dunning')
  await page.getByRole('button', { name: STATE_FILTER[run.state] ?? 'All', exact: true }).click()

  const link = page.getByRole('link', { name: invoice.invoice_number, exact: true })
  const showing = page.getByText(/^Showing \d+–\d+ of \d+$/)
  for (let p = 1; !(await link.isVisible()); p++) {
    await expect(link.or(showing)).toBeVisible()
    if (await link.isVisible()) break
    if (p >= 40) throw new Error(`run for ${invoice.invoice_number} not found in ${p} pages`)
    await page.getByLabel('Go to next page').click()
    await expect(page.getByText(new RegExp(`^Showing ${p * RUNS_PAGE_SIZE + 1}–`))).toBeVisible()
  }

  const runRow = page.getByRole('row').filter({ has: link })
  // The open/close chevron is the row's first button. It has no accessible
  // name, and this spec does not change the page.
  await runRow.getByRole('button').first().click()
  const timeline = page.getByRole('row').filter({ hasText: 'Recovery timeline' })
  await expect(timeline).toBeVisible()
  await expect(timeline.getByText('Loading timeline...')).toHaveCount(0)

  const runs = page.getByRole('main').locator('[data-slot="card"]').filter({ has: runRow })
  await atEachWidth(page, name, async file => {
    const [box, top, bottom] = await Promise.all([runs.boundingBox(), runRow.boundingBox(), timeline.boundingBox()])
    if (!box || !top || !bottom) throw new Error('the run rows are not on screen')
    await page.screenshot({
      path: file,
      animations: 'disabled',
      clip: { x: box.x, y: top.y, width: box.width, height: bottom.y + bottom.height - top.y },
    })
  })
}

for (const clock of ['real', 'test'] as const) {
  test(`activity shots, ${clock} clock: an unpaid first invoice through emails, a credit note and an offline payment (invoice, Dunning run, Sent emails)`, async ({ page }) => {
    const plan = await planBilledInAdvance()
    const { customer, testClock } = await customerOn(clock)
    await ownRetryPolicy(customer.id)
    await subscribe(customer.id, plan.id)
    // The first period is invoiced at the start. With no card on file,
    // payment recovery starts at once.
    const [first] = await invoicesFor(customer.id)
    expect(first, 'the subscription invoiced its first period').toBeTruthy()
    const invoice = first as Invoice

    await asPerson(page, 'POST', `/invoices/${invoice.id}/send`, { email: customer.email })
    const note = await api<{ id: string }>('POST', '/credit-notes', {
      invoice_id: invoice.id,
      reason: 'Billing error',
      auto_issue: true,
      lines: [{ description: 'Billing error', quantity: 1, unit_amount_cents: 1000 }],
    })
    await asPerson(page, 'POST', `/credit-notes/${note.id}/send`, { email: customer.email })
    if (testClock) {
      // Two days past the last retry (day 14), so the three retries run and
      // then retries run out.
      await advanceTo(testClock, Date.parse(FROZEN) + 16 * DAY)
    }
    // Before the payment: an email still queued when the invoice is paid is
    // not sent, and whether that happens would depend on timing.
    await waitForEmailsSent(customer.id)
    await asPerson(page, 'POST', `/invoices/${invoice.id}/record-payment`, { note: 'Wire 1042' })
    await waitForEmailsSent(customer.id)

    await page.goto(`/invoices/${invoice.id}`)
    await shootCard(page, card(page, 'Activity'), `invoice-${clock}`)
    await shootDunningRun(page, invoice, `dunning-${clock}`)
    await page.goto(`/customers/${customer.id}`)
    const sent = card(page, /^Sent emails/)
    await expect(sent).toContainText(/Sent emails \(\d+\)/)
    await shootCard(page, sent, `sent-emails-${clock}`)
  })

  test(`activity shots, ${clock} clock: a trial extended, spend thresholds, a cancellation undone, collection paused and resumed (subscription)`, async ({ page }) => {
    const plan = await planBilledInAdvance()
    const { customer, testClock } = await customerOn(clock)
    const sub = await subscribe(customer.id, plan.id, { trial_days: 14 })
    const trialEnd = Date.parse(sub.trial_end_at!) + 7 * DAY

    await asPerson(page, 'POST', `/subscriptions/${sub.id}/extend-trial`, { trial_end: iso(trialEnd) })
    await api('PUT', `/subscriptions/${sub.id}/billing-thresholds`, { amount_gte: 50000 })
    await asPerson(page, 'POST', `/subscriptions/${sub.id}/schedule-cancel`, { at_period_end: true })
    await asPerson(page, 'DELETE', `/subscriptions/${sub.id}/scheduled-cancel`)
    await api('PUT', `/subscriptions/${sub.id}/pause-collection`, { behavior: 'keep_as_draft' })
    await asPerson(page, 'DELETE', `/subscriptions/${sub.id}/pause-collection`)
    if (testClock) {
      // Two days past the trial's end, so the trial runs out on its own.
      await advanceTo(testClock, trialEnd + 2 * DAY)
    }

    await page.goto(`/subscriptions/${sub.id}`)
    await shootCard(page, card(page, 'Activity'), `subscription-${clock}`)
  })
}

// One activity row as text, for the parity check: the row's lines joined
// by " | ", with every <time> element and the Recorded part taken out, each
// link replaced by its kind, and each email address and document number
// replaced by a placeholder (they differ between the two customers).
interface RowText { text: string; recorded: boolean }

async function activityRowTexts(target: Locator): Promise<RowText[]> {
  const rows = target.locator('[data-activity-row]')
  await expect(rows.first()).toBeVisible()
  return rows.evaluateAll(els => els.map(el => {
    const row = el.cloneNode(true) as HTMLElement
    const recorded = row.querySelector('[data-activity-recorded]') !== null
    row.querySelectorAll('[data-activity-recorded], time').forEach(n => n.remove())
    row.querySelectorAll('[data-activity-link]').forEach(n => { n.textContent = `<${n.getAttribute('data-activity-link')}>` })
    const text = Array.from(row.querySelectorAll('p'))
      .map(p => (p.textContent ?? '')
        .replace(/[^\s@·]+@[^\s@·]+/g, '<email>')
        .replace(/\b[A-Z][A-Z0-9]*-\d{3,}\b/g, '<number>')
        .replace(/\s+/g, ' ')
        .replace(/\s*·\s*$/, '')
        .trim())
      .filter(Boolean)
      .join(' | ')
    return { text, recorded }
  }))
}

// The card fits the narrow width: no row is wider than the card.
async function expectNoOverflow(page: Page, target: Locator) {
  for (const width of WIDTHS) {
    await page.setViewportSize({ width, height: 900 })
    const over = await target.evaluate(el => {
      const wide = [el, ...Array.from(el.querySelectorAll('[data-activity-row]'))]
        .filter(n => n.scrollWidth > n.clientWidth + 1)
      return wide.map(n => (n.textContent ?? '').slice(0, 60))
    })
    expect(over, `nothing in the Activity card is wider than the card at ${width}px`).toEqual([])
  }
}

// The browser's zone in the parity test: far from UTC (the e2e account's
// zone) and on the other side of it, so a list that used the browser's zone
// instead of the account's would name the wrong zone and, for the frozen test
// clock (04:30 UTC), put rows under the day before.
const BROWSER_ZONE = 'Pacific/Pago_Pago'

function zoneParts(iso: string, tz: string, opts: Intl.DateTimeFormatOptions): Record<string, string> {
  const out: Record<string, string> = {}
  for (const p of new Intl.DateTimeFormat('en-US', { ...opts, timeZone: tz }).formatToParts(new Date(iso))) out[p.type] = p.value
  return out
}

// The card's times are in the account's zone, not the browser's: the header
// names the zone of every time the rows show, and each day header carries
// the account-calendar day of its first row.
async function expectAccountZone(target: Locator, tz: string) {
  const seen = await target.evaluate(el => ({
    header: Array.from(el.querySelectorAll('span')).map(s => s.textContent ?? '').find(t => t.startsWith('Times in ')) ?? '',
    times: Array.from(el.querySelectorAll('[data-activity-row] time')).map(t => t.getAttribute('datetime') ?? ''),
    days: Array.from(el.querySelectorAll('[data-activity-day]')).map(p => ({
      day: p.querySelector('time')?.getAttribute('datetime') ?? '',
      first: p.parentElement?.querySelector('[data-activity-row] time')?.getAttribute('datetime') ?? '',
    })),
  }))
  const names: string[] = []
  for (const t of seen.times) {
    const n = zoneParts(t, tz, { timeZoneName: 'short' }).timeZoneName
    if (n && !names.includes(n)) names.push(n)
  }
  expect(seen.header, `the card names the account zone (${tz}), not the browser's (${BROWSER_ZONE})`).toBe(`Times in ${names.join(' / ')}`)
  expect(seen.days.length, 'the card has day headers').toBeGreaterThan(0)
  for (const { day, first } of seen.days) {
    const p = zoneParts(first, tz, { year: 'numeric', month: '2-digit', day: '2-digit' })
    expect(day, `the day header for ${first} is its day in ${tz}`).toBe(`${p.year}-${p.month}-${p.day}`)
  }
}

// ADR-128: a test-clock line equals the real-clock line plus one Recorded
// part. The same operator story runs for a real-clock customer and for one
// on a frozen test clock; the Activity cards must read the same once times
// are taken out, every test-clock row must carry a Recorded part, and no
// real-clock row may.
test.describe('activity text parity', () => {
  test.use({ timezoneId: BROWSER_ZONE })

  test('activity text parity: one operator story on an invoice reads the same on the real clock and a test clock', async ({ page }) => {
    const tz = (await api<{ timezone?: string }>('GET', '/settings')).timezone || 'UTC'
    const read: Partial<Record<Clock, { paid: RowText[]; voided: RowText[] }>> = {}
    for (const clock of ['real', 'test'] as const) {
      const { customer } = await customerOn(clock)
      const draft = await asPerson<Invoice>(page, 'POST', '/invoices', {
        customer_id: customer.id,
        currency: 'USD',
        line_items: [{ description: 'E2E onboarding', line_type: 'add_on', quantity: 1, unit_amount_cents: 4900 }],
      })
      await asPerson(page, 'POST', `/invoices/${draft.id}/finalize`)
      // Each email is waited for before the next step. An email row is dated
      // when the mail server took it, so two emails queued together would
      // land in whichever order the worker sent them, on either clock.
      await asPerson(page, 'POST', `/invoices/${draft.id}/send`, { email: customer.email })
      await waitForEmailsSent(customer.id)
      const note = await asPerson<{ id: string }>(page, 'POST', '/credit-notes', {
        invoice_id: draft.id,
        reason: 'Billing error',
        auto_issue: true,
        lines: [{ description: 'Billing error', quantity: 1, unit_amount_cents: 1000 }],
      })
      await asPerson(page, 'POST', `/credit-notes/${note.id}/send`, { email: customer.email })
      await waitForEmailsSent(customer.id)
      await asPerson(page, 'POST', `/invoices/${draft.id}/record-payment`, { note: 'Wire 1042' })
      await waitForEmailsSent(customer.id)

      const other = await asPerson<Invoice>(page, 'POST', '/invoices', {
        customer_id: customer.id,
        currency: 'USD',
        line_items: [{ description: 'E2E setup', line_type: 'add_on', quantity: 1, unit_amount_cents: 2500 }],
      })
      await asPerson(page, 'POST', `/invoices/${other.id}/finalize`)
      await waitForEmailsSent(customer.id)
      await asPerson(page, 'POST', `/invoices/${other.id}/void`)
      await waitForEmailsSent(customer.id)

      await page.goto(`/invoices/${draft.id}`)
      const paidCard = card(page, 'Activity')
      await expect(paidCard.getByText(/^Times in /)).toBeVisible()
      const paid = await activityRowTexts(paidCard)
      await expectAccountZone(paidCard, tz)
      await expectNoOverflow(page, paidCard)
      await shootCard(page, paidCard, `invoice-operator-${clock}`)

      await page.goto(`/invoices/${other.id}`)
      const voidedCard = card(page, 'Activity')
      const voided = await activityRowTexts(voidedCard)
      await expectAccountZone(voidedCard, tz)
      await expectNoOverflow(page, voidedCard)
      read[clock] = { paid, voided }
    }

    const real = read.real!
    const sim = read.test!
    for (const key of ['paid', 'voided'] as const) {
      expect(sim[key].map(r => r.text), `${key} invoice: the test-clock rows read as the real-clock rows`).toEqual(real[key].map(r => r.text))
      expect(real[key].filter(r => r.recorded).map(r => r.text), `${key} invoice: no real-clock row has a Recorded part`).toEqual([])
      expect(sim[key].filter(r => !r.recorded).map(r => r.text), `${key} invoice: every test-clock row has a Recorded part`).toEqual([])
    }
  })
})
