import { test, expect, type Locator, type Page, type Route } from '@playwright/test'
import { api } from './fixtures/api'
import { createCustomer, type TestClock } from './fixtures/seed'

// The dashboard turns a picked day or time into an instant in the account's
// time zone, which it reads from GET /v1/settings. A signed-in page waits for
// the settings instead of guessing UTC while they load. The date picker's
// first day and its Today button are the day of the customer's now in that
// zone: a test clock's now when the customer is on one.
//
// CI's account is in UTC, the guess the dashboard used to make, so the
// browser answers the settings read with another zone.

interface Settings { timezone: string }

// A zone ahead of the browser's Los Angeles and of the account's own zone.
async function otherZone(): Promise<string> {
  const real = await api<Settings>('GET', '/settings')
  return real.timezone === 'Asia/Tokyo' ? 'Asia/Kolkata' : 'Asia/Tokyo'
}

// route.fetch sends the request from outside the page, without the
// Sec-Fetch-Site header Chrome adds, and the server's CSRF check would refuse
// it. Send the header as Chrome does.
function fetchFromPage(route: Route) {
  return route.fetch({ headers: { ...route.request().headers(), 'sec-fetch-site': 'same-origin' } })
}

// Answers the settings reads with `zone`. While `held` is true, an answer
// waits until release() is called.
async function settingsIn(page: Page, zone: string, { held = false } = {}): Promise<() => void> {
  let release = () => {}
  const released = held ? new Promise<void>(resolve => { release = resolve }) : Promise.resolve()
  await page.route(url => url.pathname === '/v1/settings', async route => {
    if (route.request().method() !== 'GET') return route.fallback()
    await released
    const answer = await fetchFromPage(route)
    await route.fulfill({ response: answer, json: { ...(await answer.json()), timezone: zone } })
  })
  return () => release()
}

// The yyyy-mm-dd day of an instant in a zone.
function dayIn(instant: Date, timeZone: string): string {
  return new Intl.DateTimeFormat('en-CA', { year: 'numeric', month: '2-digit', day: '2-digit', timeZone }).format(instant)
}

// The instant of 00:00 on the day after `day` in the zone, found by stepping
// a UTC guess by the zone's offset at that moment.
function nextMidnightIn(day: string, timeZone: string): string {
  const [y, m, d] = day.split('-').map(Number)
  const utcMidnight = Date.UTC(y, m - 1, d + 1)
  const wall = new Intl.DateTimeFormat('en-CA', {
    year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hourCycle: 'h23', timeZone,
  }).formatToParts(new Date(utcMidnight))
  const part = (type: string) => Number(wall.find(p => p.type === type)!.value)
  const offsetMs = Date.UTC(part('year'), part('month') - 1, part('day'), part('hour'), part('minute')) - utcMidnight
  return new Date(utcMidnight - offsetMs).toISOString()
}

// Opens Grant credits on the customer's page.
async function openGrantCredits(page: Page) {
  const credits = page.locator('section').filter({ has: page.getByRole('heading', { name: 'Credits', exact: true }) })
  await credits.getByRole('button', { name: 'Grant credits' }).click()
  return page.getByRole('dialog', { name: 'Grant credits' })
}

// Grants $20 from the open dialog and returns the expires_at it sent.
async function grantAndReadExpiry(page: Page, dialog: Locator): Promise<string | undefined> {
  await dialog.getByLabel(/^Amount/).fill('20.00')
  await dialog.getByLabel('Description').fill('E2E zone credit')
  await dialog.getByRole('button', { name: 'Grant credits' }).click()
  const granting = page.waitForResponse(r => r.request().method() === 'POST' && new URL(r.url()).pathname === '/v1/credits/grant')
  await page.getByRole('alertdialog', { name: 'Confirm credit grant' }).getByRole('button', { name: 'Grant credits' }).click()
  const res = await granting
  expect(res.status()).toBe(201)
  return (res.request().postDataJSON() as { expires_at?: string }).expires_at
}

test('a signed-in page waits for the account settings, then a credit expiry uses their zone', async ({ page }) => {
  const customer = await createCustomer()
  const zone = await otherZone()
  const release = await settingsIn(page, zone, { held: true })
  await page.goto(`/customers/${customer.id}`)

  // No zone yet: the page waits, so nothing on it can send a guessed one.
  await expect(page.getByRole('status', { name: 'Loading your account settings' })).toBeVisible()
  await expect(page.getByRole('heading', { level: 1, name: customer.display_name, exact: true })).toHaveCount(0)

  release()
  await expect(page.getByRole('heading', { level: 1, name: customer.display_name, exact: true })).toBeVisible()
  const dialog = await openGrantCredits(page)
  // 40 days ahead in the zone: always a future day.
  const picked = dayIn(new Date(Date.now() + 40 * 86_400_000), zone)
  await dialog.getByPlaceholder('Never expires').fill(picked)
  await dialog.getByPlaceholder('Never expires').press('Enter')
  expect(await grantAndReadExpiry(page, dialog), `valid through ${picked} ends at the next midnight in ${zone}`).toBe(nextMidnightIn(picked, zone))
})

test('a page says when the account settings did not load, and Retry loads them', async ({ page }) => {
  const customer = await createCustomer()
  let failing = true
  await page.route(url => url.pathname === '/v1/settings', async route => {
    if (route.request().method() !== 'GET') return route.fallback()
    if (failing) return route.fulfill({ status: 500, body: '{}' })
    return route.fulfill({ response: await fetchFromPage(route) })
  })
  await page.goto(`/customers/${customer.id}`)
  const failed = page.getByRole('alert').filter({ hasText: "Couldn't load your account settings" })
  await expect(failed).toBeVisible()
  await expect(page.getByRole('heading', { level: 1, name: customer.display_name, exact: true })).toHaveCount(0)

  failing = false
  await failed.getByRole('button', { name: 'Retry' }).click()
  await expect(page.getByRole('heading', { level: 1, name: customer.display_name, exact: true })).toBeVisible()
  await expect(failed).toHaveCount(0)
})

test.describe('a browser behind the account zone', () => {
  test.use({ timezoneId: 'America/Los_Angeles' })

  test("on a test clock, the expiry picker's first day and Today are the clock's day in the account zone", async ({ page }) => {
    // 20:00 UTC on Jan 15 is noon on Jan 15 in Los Angeles, but already
    // Jan 16 in the account zone (Tokyo or Kolkata).
    const frozen = '2026-01-15T20:00:00Z'
    const clock = await api<TestClock>('POST', '/test-clocks', { name: 'E2E zone clock', frozen_time: frozen })
    const customer = await createCustomer(clock.id)
    const zone = await otherZone()
    await settingsIn(page, zone)
    const clockDay = dayIn(new Date(frozen), zone)
    expect(clockDay).toBe('2026-01-16')

    await page.goto(`/customers/${customer.id}`)
    const dialog = await openGrantCredits(page)
    await dialog.getByRole('button', { name: 'Open calendar' }).click()
    const calendar = page.getByRole('grid')
    // Jan 15 has ended in the account zone, so it cannot be picked.
    await expect(calendar.getByRole('button', { name: 'Thursday, January 15th, 2026' })).toBeDisabled()
    await expect(calendar.getByRole('button', { name: 'Today, Friday, January 16th, 2026' })).toBeEnabled()

    await page.getByRole('button', { name: 'Today', exact: true }).click()
    await expect(dialog.getByPlaceholder('Never expires')).toHaveValue('Jan 16, 2026')
    expect(await grantAndReadExpiry(page, dialog), `valid through the clock's day ends at the next midnight in ${zone}`).toBe(nextMidnightIn(clockDay, zone))
  })
})
