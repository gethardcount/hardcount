import { test, expect, type Page, type Route } from '@playwright/test'
import { api, uniq } from './fixtures/api'
import { createCustomer } from './fixtures/seed'

// Signed-in pages wait for the account settings (GET /v1/settings), and a
// Settings save reaches every page at once, in both modes. Create plan and
// Grant credits use the account's default currency, so a page drawn before
// the settings load would have to guess it.
//
// CI's account is in USD, the likeliest guess, so the browser answers the
// settings read with another currency. A real Settings save would change the
// account under the tests running beside these, so the browser answers the
// save too.

interface Settings { default_currency: string; timezone: string }

// The real settings with `change` applied, sent only once release() is called.
async function holdSettings(page: Page, change: Partial<Settings>): Promise<() => void> {
  let release!: () => void
  const released = new Promise<void>(resolve => { release = resolve })
  await page.route(url => url.pathname === '/v1/settings', async route => {
    if (route.request().method() !== 'GET') return route.fallback()
    await released
    const answer = await fetchFromPage(route)
    await route.fulfill({ response: answer, json: { ...(await answer.json()), ...change } })
  })
  return release
}

// route.fetch sends the request from outside the page, without the
// Sec-Fetch-Site header Chrome adds, and the server's CSRF check would refuse
// it. Send the header as Chrome does.
function fetchFromPage(route: Route) {
  return route.fetch({ headers: { ...route.request().headers(), 'sec-fetch-site': 'same-origin' } })
}

// A zone and a default currency that differ from the account's.
function otherSettings(real: Settings) {
  return {
    zone: real.timezone === 'Asia/Kolkata' ? 'America/Denver' : 'Asia/Kolkata',
    currency: real.default_currency === 'EUR'
      ? { code: 'GBP', option: /British Pound \(GBP\)/, symbol: '£' }
      : { code: 'EUR', option: /Euro \(EUR\)/, symbol: '€' },
  }
}

// Answers the Settings save in the browser. A settings read after it waits
// until endOfTest() is called, so the pages can only use what the save itself
// returned. Reads before the save reach the server.
async function answerSave(page: Page, real: Settings): Promise<() => void> {
  let saved: Settings | undefined
  let endOfTest!: () => void
  const ended = new Promise<void>(resolve => { endOfTest = resolve })
  await page.route(url => url.pathname === '/v1/settings', async route => {
    if (route.request().method() === 'PUT') {
      saved = { ...real, ...(route.request().postDataJSON() as Partial<Settings>) }
      return route.fulfill({ json: saved })
    }
    if (!saved) return route.fallback()
    await ended
    return route.fulfill({ json: saved })
  })
  return endOfTest
}

// Picks an option in a searchable picker on the Settings page.
async function pick(page: Page, label: string, search: string, option: RegExp) {
  await page.getByText(label, { exact: true }).locator('..').getByRole('button').first().click()
  await page.getByPlaceholder('Search...', { exact: true }).fill(search)
  await page.getByRole('option', { name: option }).click()
}

// On the Settings page: saves a new time zone and default currency.
async function saveZoneAndCurrency(page: Page, zone: string, currency: ReturnType<typeof otherSettings>['currency']) {
  await pick(page, 'Timezone', zone.split('/')[1], new RegExp(`^${zone}`))
  await page.getByRole('tab', { name: 'Invoicing' }).click()
  await pick(page, 'Default currency', currency.code, currency.option)
  const saving = page.waitForRequest(r => r.method() === 'PUT' && new URL(r.url()).pathname === '/v1/settings')
  await page.getByRole('button', { name: 'Save changes' }).click()
  expect((await saving).postDataJSON()).toMatchObject({ timezone: zone, default_currency: currency.code })
  await expect(page.getByRole('region', { name: /^Notifications/ }).getByText('Settings saved', { exact: true })).toBeVisible()
}

// Opens a page from the sidebar, without a reload, so the query cache stays.
async function openFromSidebar(page: Page, name: string) {
  const link = name === 'Settings'
    ? page.getByRole('link', { name: 'Settings', exact: true })
    : page.getByRole('navigation').getByRole('link', { name, exact: true })
  await link.click()
}

// The Usage page's default window starts at a midnight in the zone.
async function expectUsageWindowIn(page: Page, zone: string) {
  const listing = page.waitForRequest(r => new URL(r.url()).pathname === '/v1/usage-events' && new URL(r.url()).searchParams.has('from'))
  await openFromSidebar(page, 'Usage')
  const from = new URL((await listing).url()).searchParams.get('from')
  expect(from && midnightIn(from, zone), `usage window starts ${from}, a midnight in ${zone}`).toBe(true)
}

// Create plan names the currency the plan is created in.
async function expectCreatePlanIn(page: Page, symbol: string) {
  await openFromSidebar(page, 'Pricing')
  await page.getByRole('button', { name: 'Add Plan' }).click()
  const dialog = page.getByRole('dialog', { name: 'Create plan' })
  await expect(dialog.getByText(`Base Price (${symbol})`, { exact: true })).toBeVisible()
  await expect(dialog.getByRole('button', { name: 'Create plan' })).toBeEnabled()
  await dialog.getByRole('button', { name: 'Cancel' }).click()
}

// True when the instant is a midnight in the zone.
function midnightIn(instant: string, timeZone: string): boolean {
  const time = new Intl.DateTimeFormat('en-GB', { hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23', timeZone })
  return time.format(new Date(instant)) === '00:00:00'
}

// Opens Grant credits on the customer's page, which is open.
async function openGrantCredits(page: Page) {
  const credits = page.locator('section').filter({ has: page.getByRole('heading', { name: 'Credits', exact: true }) })
  await credits.getByRole('button', { name: 'Grant credits' }).click()
  return page.getByRole('dialog', { name: 'Grant credits' })
}

// The wait a signed-in page shows until the account settings load.
function settingsWait(page: Page) {
  return page.getByRole('status', { name: 'Loading your account settings' })
}

test('the Pricing page waits for the account settings, then Create plan uses their currency', async ({ page }) => {
  const release = await holdSettings(page, { default_currency: 'EUR' })
  await page.goto('/pricing')
  // The settings have not answered: no currency, so no plan can be started.
  await expect(settingsWait(page)).toBeVisible()
  await expect(page.getByRole('button', { name: 'Add Plan' })).toHaveCount(0)

  release()
  await page.getByRole('button', { name: 'Add Plan' }).click()
  const dialog = page.getByRole('dialog', { name: 'Create plan' })
  const id = uniq()
  await dialog.getByText('Plan Name', { exact: true }).locator('..').getByRole('textbox').fill(`E2E EUR plan ${id}`)
  await dialog.getByLabel('Code', { exact: true }).fill(`e2e-eur-${id}`)
  await dialog.getByText(/^Base Price/).locator('..').getByRole('textbox').fill('10.00')
  await expect(dialog.getByText('Base Price (€)', { exact: true })).toBeVisible()
  const creating = page.waitForResponse(r => r.request().method() === 'POST' && new URL(r.url()).pathname === '/v1/plans')
  await dialog.getByRole('button', { name: 'Create plan' }).click()
  const res = await creating
  expect((res.request().postDataJSON() as { currency: string }).currency).toBe('EUR')
  expect(res.status()).toBe(201)
  const plan = (await res.json()) as { id: string; currency: string }
  expect(plan.currency).toBe('EUR')
  await expect(page).toHaveURL(new RegExp(`/plans/${plan.id}$`))
})

test('the customer page waits for the account settings, then Grant credits uses their currency', async ({ page }) => {
  // No billing profile, subscription or invoice: the grant defaults to the
  // account currency.
  const customer = await createCustomer()
  const release = await holdSettings(page, { default_currency: 'EUR' })
  await page.goto(`/customers/${customer.id}`)
  await expect(settingsWait(page)).toBeVisible()
  await expect(page.getByRole('heading', { level: 1, name: customer.display_name, exact: true })).toHaveCount(0)

  release()
  const dialog = await openGrantCredits(page)
  await dialog.getByLabel(/^Amount/).fill('25.00')
  await dialog.getByLabel('Description').fill('E2E account-currency credit')
  await expect(dialog.getByText('Amount (€)', { exact: true })).toBeVisible()
  await expect(dialog.getByText("Loading this customer's currency…", { exact: true })).toHaveCount(0)
  await dialog.getByRole('button', { name: 'Grant credits' }).click()
  const confirm = page.getByRole('alertdialog', { name: 'Confirm credit grant' })
  await expect(confirm).toContainText('Grant €25.00 EUR to')
  const granting = page.waitForResponse(r => r.request().method() === 'POST' && new URL(r.url()).pathname === '/v1/credits/grant')
  await confirm.getByRole('button', { name: 'Grant credits' }).click()
  const res = await granting
  expect((res.request().postDataJSON() as { currency: string }).currency).toBe('EUR')
  expect(res.status()).toBe(201)

  const balances = await api<{ data: { currency: string; balance_cents: number }[] }>('GET', `/credits/balance/${customer.id}`)
  expect(balances.data.map(b => [b.currency, b.balance_cents])).toEqual([['EUR', 2500]])
})

test("Grant credits says when the customer's currency did not load, and Retry loads it", async ({ page }) => {
  // No currency of their own, so the answer is the account's EUR once the
  // customer's billing profile read shows they have none. That read fails
  // until the test lets it through.
  const customer = await createCustomer()
  const release = await holdSettings(page, { default_currency: 'EUR' })
  release()
  let failing = true
  await page.route(url => url.pathname === `/v1/customers/${customer.id}/billing-profile`, async route => {
    if (route.request().method() !== 'GET') return route.fallback()
    if (failing) return route.fulfill({ status: 500, body: '{}' })
    return route.fallback()
  })
  await page.goto(`/customers/${customer.id}`)
  const dialog = await openGrantCredits(page)
  const failed = dialog.getByRole('alert').filter({ hasText: "Couldn't load this customer's currency" })
  await expect(failed).toBeVisible()
  await expect(failed).toContainText('You can also pick a currency above.')
  await expect(dialog.getByRole('button', { name: 'Grant credits' })).toBeDisabled()

  failing = false
  await failed.getByRole('button', { name: 'Retry' }).click()
  await expect(dialog.getByText('Amount (€)', { exact: true })).toBeVisible()
  await expect(failed).toHaveCount(0)
  await expect(dialog.getByRole('button', { name: 'Grant credits' })).toBeEnabled()
})

test('a Settings save reaches the other pages without a reload', async ({ page }) => {
  const real = await api<Settings>('GET', '/settings')
  const { zone, currency } = otherSettings(real)
  const endOfTest = await answerSave(page, real)

  await page.goto('/settings')
  await saveZoneAndCurrency(page, zone, currency)
  await expectUsageWindowIn(page, zone)
  await expectCreatePlanIn(page, currency.symbol)
  endOfTest()
})

test('a Settings save reaches the other mode too', async ({ page }) => {
  // Every test shares one session, and the server keeps its mode, so the
  // browser answers the switch. Only the dashboard switches: each mode has
  // its own query cache, and the cache of the mode left is kept. That cache
  // is what this test is about.
  await page.route(url => url.pathname === '/v1/auth/mode', route =>
    route.fulfill({ json: { livemode: (route.request().postDataJSON() as { livemode: boolean }).livemode } }))
  const switchTo = async (mode: 'Test' | 'Live') => {
    await page.getByRole('radio', { name: `${mode} mode` }).click()
    await expect(page.getByRole('radio', { name: `${mode} mode` })).toHaveAttribute('aria-checked', 'true')
  }
  const real = await api<Settings>('GET', '/settings')
  const { zone, currency } = otherSettings(real)
  const endOfTest = await answerSave(page, real)

  // Test mode's cache holds the account's settings.
  await page.goto('/pricing')
  await page.getByRole('button', { name: 'Add Plan' }).click()
  const dialog = page.getByRole('dialog', { name: 'Create plan' })
  await expect(dialog.getByText(/^Base Price \(.+\)$/)).toBeVisible()
  await dialog.getByRole('button', { name: 'Cancel' }).click()

  // Saved in live mode, then back to test mode within the cache's 30 s.
  await switchTo('Live')
  await openFromSidebar(page, 'Settings')
  await saveZoneAndCurrency(page, zone, currency)
  await switchTo('Test')

  await expectCreatePlanIn(page, currency.symbol)
  await expectUsageWindowIn(page, zone)
  endOfTest()
})

test("a new default currency reloads Home's figures", async ({ page }) => {
  // Home's figures and revenue chart are in the default currency, so a new
  // one reads them again, within the cache's 30 s too.
  const real = await api<Settings>('GET', '/settings')
  const { zone, currency } = otherSettings(real)
  const endOfTest = await answerSave(page, real)
  const reads = { overview: 0, chart: 0 }
  page.on('request', r => {
    const url = new URL(r.url())
    if (url.pathname === '/v1/analytics/overview' && url.searchParams.has('period')) reads.overview++
    if (url.pathname === '/v1/analytics/revenue-chart') reads.chart++
  })

  await page.goto('/')
  await expect(page.getByRole('heading', { level: 1, name: 'Home' })).toBeVisible()
  await expect.poll(() => reads).toEqual({ overview: 1, chart: 1 })
  await openFromSidebar(page, 'Settings')
  await saveZoneAndCurrency(page, zone, currency)
  await openFromSidebar(page, 'Home')
  await expect(page.getByRole('heading', { level: 1, name: 'Home' })).toBeVisible()
  await expect.poll(() => reads).toEqual({ overview: 2, chart: 2 })
  endOfTest()
})
