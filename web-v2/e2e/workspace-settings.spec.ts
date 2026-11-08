import { test, expect, type Page, type Route } from '@playwright/test'
import { api, uniq } from './fixtures/api'
import { createCustomer } from './fixtures/seed'

// Money writes on the dashboard wait for the account settings
// (GET /v1/settings), and a Settings save reaches every page at once. Create
// plan and Grant credits use the account's default currency, so until the
// settings load, either would have to guess it.
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

// Picks an option in a searchable picker on the Settings page.
async function pick(page: Page, label: string, search: string, option: RegExp) {
  await page.getByText(label, { exact: true }).locator('..').getByRole('button').first().click()
  await page.getByPlaceholder('Search...', { exact: true }).fill(search)
  await page.getByRole('option', { name: option }).click()
}

// True when the instant is a midnight in the zone.
function midnightIn(instant: string, timeZone: string): boolean {
  const time = new Intl.DateTimeFormat('en-GB', { hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23', timeZone })
  return time.format(new Date(instant)) === '00:00:00'
}

test('Create plan waits for the account currency, then creates the plan in it', async ({ page }) => {
  const release = await holdSettings(page, { default_currency: 'EUR' })
  await page.goto('/pricing')
  await page.getByRole('button', { name: 'Add Plan' }).click()
  const dialog = page.getByRole('dialog', { name: 'Create plan' })
  const id = uniq()
  await dialog.getByText('Plan Name', { exact: true }).locator('..').getByRole('textbox').fill(`E2E EUR plan ${id}`)
  await dialog.getByLabel('Code', { exact: true }).fill(`e2e-eur-${id}`)
  await dialog.getByText(/^Base Price/).locator('..').getByRole('textbox').fill('10.00')

  // The settings have not answered: no currency, so nothing can be created.
  await expect(dialog.getByRole('button', { name: 'Create plan' })).toBeDisabled()
  await expect(dialog.getByText('Base Price', { exact: true })).toBeVisible()
  await expect(dialog.getByText("Loading the account's currency…", { exact: true })).toBeVisible()

  release()
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

test('Grant credits waits for the account currency of a customer with none of their own', async ({ page }) => {
  // No billing profile, subscription or invoice: the grant defaults to the
  // account currency.
  const customer = await createCustomer()
  const release = await holdSettings(page, { default_currency: 'EUR' })
  await page.goto(`/customers/${customer.id}`)
  const credits = page.locator('section').filter({ has: page.getByRole('heading', { name: 'Credits', exact: true }) })
  await credits.getByRole('button', { name: 'Grant credits' }).click()
  const dialog = page.getByRole('dialog', { name: 'Grant credits' })
  await dialog.getByLabel(/^Amount/).fill('25.00')
  await dialog.getByLabel('Description').fill('E2E account-currency credit')
  await expect(dialog.getByRole('button', { name: 'Grant credits' })).toBeDisabled()

  release()
  await expect(dialog.getByText('Amount (€)', { exact: true })).toBeVisible()
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

test('a Settings save reaches the other pages without a reload', async ({ page }) => {
  const real = await api<Settings>('GET', '/settings')
  const zone = real.timezone === 'Asia/Kolkata' ? 'America/Denver' : 'Asia/Kolkata'
  const currency = real.default_currency === 'EUR' ? { code: 'GBP', option: /British Pound \(GBP\)/, symbol: '£' } : { code: 'EUR', option: /Euro \(EUR\)/, symbol: '€' }
  // The save is answered here. A read after it waits until the end of the
  // test, so the pages below can only use what the save itself returned.
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

  await page.goto('/settings')
  await pick(page, 'Timezone', zone.split('/')[1], new RegExp(`^${zone}`))
  await page.getByRole('tab', { name: 'Invoicing' }).click()
  await pick(page, 'Default currency', currency.code, currency.option)
  const saving = page.waitForRequest(r => r.method() === 'PUT' && new URL(r.url()).pathname === '/v1/settings')
  await page.getByRole('button', { name: 'Save changes' }).click()
  expect((await saving).postDataJSON()).toMatchObject({ timezone: zone, default_currency: currency.code })
  await expect(page.getByRole('region', { name: /^Notifications/ }).getByText('Settings saved', { exact: true })).toBeVisible()

  // Usage's default window starts at a midnight in the saved zone.
  const listing = page.waitForRequest(r => new URL(r.url()).pathname === '/v1/usage-events' && new URL(r.url()).searchParams.has('from'))
  await page.getByRole('navigation').getByRole('link', { name: 'Usage', exact: true }).click()
  const from = new URL((await listing).url()).searchParams.get('from')
  expect(from && midnightIn(from, zone), `usage window starts ${from}, a midnight in ${zone}`).toBe(true)

  // A new plan is in the saved currency.
  await page.getByRole('navigation').getByRole('link', { name: 'Pricing', exact: true }).click()
  await page.getByRole('button', { name: 'Add Plan' }).click()
  const dialog = page.getByRole('dialog', { name: 'Create plan' })
  await expect(dialog.getByText(`Base Price (${currency.symbol})`, { exact: true })).toBeVisible()
  endOfTest()
})
