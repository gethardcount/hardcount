import { test, expect } from '@playwright/test'

// Every signed-in page renders inside one shell: the sidebar, the mode
// banner and the phone top bar. The shell is mounted once and stays while
// the operator moves between pages. Before, each page drew its own copy, so
// the shell and its data were rebuilt on every page change, and a page still
// downloading showed a full-window spinner with no sidebar.

test.describe('desktop', () => {
  test.use({ viewport: { width: 1440, height: 900 } })

  test('the sidebar stays in place when the page changes', async ({ page }) => {
    await page.goto('/customers')
    await expect(page.getByRole('heading', { level: 1, name: 'Customers', exact: true })).toBeVisible()
    const nav = page.getByRole('navigation', { name: 'Main navigation' })
    // A mark on the sidebar element survives only if the element does.
    await nav.evaluate(el => el.setAttribute('data-e2e-mark', 'kept'))

    await nav.getByRole('link', { name: 'Invoices' }).click()
    await expect(page.getByRole('heading', { level: 1, name: 'Invoices', exact: true })).toBeVisible()
    await nav.getByRole('link', { name: 'Pricing' }).click()
    await expect(page.getByRole('heading', { level: 1, name: 'Pricing', exact: true })).toBeVisible()

    await expect(nav).toHaveAttribute('data-e2e-mark', 'kept')
    await expect(nav.getByRole('link', { name: 'Pricing' })).toHaveAttribute('aria-current', 'page')
  })

  test('a new page opens at the top', async ({ page }) => {
    // Short enough that Settings scrolls.
    await page.setViewportSize({ width: 1440, height: 500 })
    await page.goto('/settings')
    await expect(page.getByRole('heading', { level: 1, name: 'Settings', exact: true })).toBeVisible()
    // Scroll to the bottom once the form has loaded and made the page long.
    const main = page.getByRole('main')
    await expect.poll(() => main.evaluate(el => {
      el.scrollTo({ top: el.scrollHeight })
      return el.scrollTop
    })).toBeGreaterThan(0)

    await page.getByRole('navigation', { name: 'Main navigation' }).getByRole('link', { name: 'Customers' }).click()
    await expect(page.getByRole('heading', { level: 1, name: 'Customers', exact: true })).toBeVisible()
    expect(await main.evaluate(el => el.scrollTop)).toBe(0)
  })

  test('a page change refreshes sidebar counts older than 30 seconds', async ({ page }) => {
    // The browser's clock, so the test can move it 30 seconds on at once.
    await page.clock.install()
    let openInvoices = 3
    await page.route(url => url.pathname === '/v1/analytics/overview', async route => {
      const response = await route.fetch()
      await route.fulfill({ response, json: { ...(await response.json()), open_invoices: openInvoices } })
    })
    await page.goto('/customers')
    const nav = page.getByRole('navigation', { name: 'Main navigation' })
    await expect(nav.getByRole('link', { name: 'Invoices' })).toHaveText('Invoices3')

    openInvoices = 4
    await page.clock.fastForward('00:31')
    await nav.getByRole('link', { name: 'Pricing' }).click()
    await expect(page.getByRole('heading', { level: 1, name: 'Pricing', exact: true })).toBeVisible()
    await expect(nav.getByRole('link', { name: 'Invoices' })).toHaveText('Invoices4')
    // The setup checklist asks the same endpoint and may still be waiting:
    // let its answer through before the page closes.
    await page.unrouteAll({ behavior: 'wait' })
  })

  test('a page that is still downloading loads inside the sidebar', async ({ page }) => {
    // Hold the Invoices page's code until the sidebar has been checked.
    let release!: () => void
    const held = new Promise<void>(resolve => { release = resolve })
    let holds = 0
    await page.route(/\/assets\/Invoices-[\w-]+\.js$/, async route => {
      holds++
      await held
      await route.continue()
    })

    await page.goto('/invoices', { waitUntil: 'commit' })
    await expect(page.getByRole('navigation', { name: 'Main navigation' })).toBeVisible()
    await expect.poll(() => holds).toBe(1)
    const heading = page.getByRole('heading', { level: 1, name: 'Invoices', exact: true })
    await expect(heading).toHaveCount(0)

    release()
    await expect(heading).toBeVisible()
  })
})

test.describe('phone', () => {
  test.use({ viewport: { width: 390, height: 844 } })

  test('the menu closes when the page changes, also on Back', async ({ page }) => {
    await page.goto('/customers')
    await expect(page.getByRole('heading', { level: 1, name: 'Customers', exact: true })).toBeVisible()
    const open = page.getByRole('button', { name: 'Open menu' })
    const close = page.getByRole('button', { name: 'Close menu' })

    // A link in the menu.
    await open.click()
    await expect(close).toBeInViewport()
    await page.getByRole('navigation', { name: 'Main navigation' }).getByRole('link', { name: 'Invoices' }).click()
    await expect(page.getByRole('heading', { level: 1, name: 'Invoices', exact: true })).toBeVisible()
    await expect(close).not.toBeInViewport()

    // The browser's Back button, with the menu open.
    await open.click()
    await expect(close).toBeInViewport()
    await page.goBack()
    await expect(page.getByRole('heading', { level: 1, name: 'Customers', exact: true })).toBeVisible()
    await expect(close).not.toBeInViewport()
  })
})
