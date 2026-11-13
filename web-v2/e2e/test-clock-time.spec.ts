import { test, expect, type Page } from '@playwright/test'
import { api, uniq } from './fixtures/api'
import { createClock, createCustomer, type Invoice, type TestClock } from './fixtures/seed'

// A page counts a record's dates from its test clock's time, which it takes
// from the dashboard's one cached list of clocks. Each test changes or adds
// a clock while that list is cached, then moves between pages only by
// clicking in the app, so no reload refreshes the cache behind the test.

// An open one-off invoice for a new customer on the clock, due 10 days after
// it is finalized (finalizing dates it at the clock's time).
async function openInvoiceOn(clockId: string): Promise<Invoice> {
  const customer = await createCustomer(clockId)
  const draft = await api<Invoice>('POST', '/invoices', {
    customer_id: customer.id,
    currency: 'USD',
    net_payment_term_days: 10,
    line_items: [{ description: 'E2E setup fee', line_type: 'add_on', quantity: 1, unit_amount_cents: 1000 }],
  })
  await api('POST', `/invoices/${draft.id}/finalize`)
  const { invoice } = await api<{ invoice: Invoice }>('GET', `/invoices/${draft.id}`)
  return invoice
}

// Opens Invoices from the sidebar and finds the invoice's row.
async function invoiceRow(page: Page, invoice: Invoice) {
  await page.getByRole('navigation', { name: 'Main navigation' }).getByRole('link', { name: 'Invoices' }).click()
  await expect(page.getByRole('heading', { level: 1, name: 'Invoices', exact: true })).toBeVisible()
  await page.getByPlaceholder('Search by number, customer or email').fill(invoice.invoice_number)
  return page.getByRole('row').filter({ hasText: invoice.invoice_number })
}

// A clock frozen 60 days ago. From real time an invoice due 10 days after
// it reads Past due; from the clock's time it reads Due in.
async function clockSixtyDaysAgo(): Promise<TestClock> {
  const frozen = new Date(Date.now() - 60 * 86_400_000).toISOString().replace(/\.\d+Z$/, 'Z')
  return api<TestClock>('POST', '/test-clocks', { name: `E2E Clock ${uniq()}`, frozen_time: frozen })
}

const isClockList = (url: URL) => url.pathname === '/v1/test-clocks'

async function openInvoice(page: Page, invoice: Invoice) {
  await page.getByRole('link', { name: invoice.invoice_number, exact: true }).click()
  await expect(page.getByRole('heading', { level: 1, name: invoice.invoice_number, exact: true })).toBeVisible()
}

test('after an advance, the invoices list and the invoice count the due date from the new clock time', async ({ page }) => {
  const clock = await createClock()
  const invoice = await openInvoiceOn(clock.id)

  await page.goto('/customers')
  let row = await invoiceRow(page, invoice)
  await expect(row.getByText(/^Due in \d+d$/)).toBeVisible()
  await openInvoice(page, invoice)
  await expect(page.getByText(/^Due in \d+d$/).first()).toBeVisible()

  // The banner's link opens the clock's page; a month on passes the due date.
  await page.getByRole('link', { name: 'View clock' }).click()
  await expect(page.getByRole('heading', { level: 1, name: clock.name })).toBeVisible()
  await page.getByRole('button', { name: 'Advance', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Advance clock' })
  await dialog.getByRole('button', { name: '+1mo' }).click()
  await dialog.getByRole('button', { name: 'Advance clock' }).click()
  await expect(dialog).toBeHidden()
  await expect(page.getByText('Last advance results')).toBeVisible({ timeout: 30_000 })

  row = await invoiceRow(page, invoice)
  await expect(row.getByText(/^Past due \d+d$/)).toBeVisible()
  await openInvoice(page, invoice)
  await expect(page.getByText(/^Past due \d+d$/).first()).toBeVisible()
  await expect(page.getByText(/^Due in \d+d$/)).toHaveCount(0)
})

test('an invoice on a clock made after the clock list was read shows its clock, not a deleted one', async ({ page }) => {
  // The test clocks page reads the clock list into the cache.
  const listRead = page.waitForResponse(res => new URL(res.url()).pathname === '/v1/test-clocks' && res.ok())
  await page.goto('/test-clocks')
  await expect(page.getByRole('heading', { level: 1, name: 'Test clocks', exact: true })).toBeVisible()
  await listRead

  const clock = await clockSixtyDaysAgo()
  const invoice = await openInvoiceOn(clock.id)

  await invoiceRow(page, invoice)
  await openInvoice(page, invoice)
  await expect(page.getByRole('link', { name: 'View clock' })).toHaveAttribute('href', `/test-clocks/${clock.id}`)
  await expect(page.getByText('Simulated time', { exact: true })).toBeVisible()
  await expect(page.getByText('Test clock deleted.')).toHaveCount(0)
  await expect(page.getByText(/^Due in \d+d$/).first()).toBeVisible()
  await expect(page.getByText(/^Past due \d+d$/)).toHaveCount(0)
})

test('while the clock list loads, the invoice shows its stored status and no due countdown', async ({ page }) => {
  const clock = await clockSixtyDaysAgo()
  const invoice = await openInvoiceOn(clock.id)

  // Hold the clock list until the page has been checked.
  let release!: () => void
  const held = new Promise<void>(resolve => { release = resolve })
  let holds = 0
  await page.route(isClockList, async route => {
    holds++
    await held
    await route.continue()
  })

  await page.goto(`/invoices/${invoice.id}`)
  await expect(page.getByRole('heading', { level: 1, name: invoice.invoice_number, exact: true })).toBeVisible()
  await expect.poll(() => holds).toBeGreaterThan(0)
  const summary = page.getByTestId('invoice-summary')
  await expect(summary.getByText('Open', { exact: true })).toBeVisible()
  await expect(page.getByText(/^Past due/)).toHaveCount(0)
  await expect(page.getByText(/^Due in \d+d$/)).toHaveCount(0)

  release()
  await expect(page.getByText(/^Due in \d+d$/).first()).toBeVisible()
  await expect(page.getByText(/^Past due/)).toHaveCount(0)
  await page.unrouteAll({ behavior: 'wait' })
})

test('when the clock list fails to load, the invoice shows that load error and no due countdown', async ({ page }) => {
  const clock = await clockSixtyDaysAgo()
  const invoice = await openInvoiceOn(clock.id)

  let failing = true
  await page.route(isClockList, async route => {
    if (failing) await route.fulfill({ status: 500, json: { error: { type: 'api_error', message: 'internal error' } } })
    else await route.continue()
  })

  await page.goto(`/invoices/${invoice.id}`)
  await expect(page.getByRole('heading', { level: 1, name: invoice.invoice_number, exact: true })).toBeVisible()
  const loadError = page.getByRole('alert').filter({ hasText: "Couldn't load test clocks" })
  await expect(loadError).toHaveCount(1)
  await expect(page.getByTestId('invoice-summary').getByText('Open', { exact: true })).toBeVisible()
  await expect(page.getByText(/^Past due/)).toHaveCount(0)
  await expect(page.getByText(/^Due in \d+d$/)).toHaveCount(0)

  // Retry reads the list again, and the countdown comes from the clock.
  failing = false
  await loadError.getByRole('button', { name: 'Retry' }).click()
  await expect(loadError).toHaveCount(0)
  await expect(page.getByText(/^Due in \d+d$/).first()).toBeVisible()
  await expect(page.getByText(/^Past due/)).toHaveCount(0)
})

test('when the clock page sees an advance finish, the clock list reads Ready', async ({ page }) => {
  const clock = await createClock()
  // The first read of the list and of the clock answer as if made during an
  // advance; later reads reach the server, where the clock is ready.
  let listReads = 0
  let clockReads = 0
  await page.route(isClockList, async route => {
    const response = await route.fetch()
    const body = await response.json() as { data: TestClock[] }
    if (listReads++ === 0) body.data = body.data.map(c => (c.id === clock.id ? { ...c, status: 'advancing' } : c))
    await route.fulfill({ response, json: body })
  })
  await page.route(url => url.pathname === `/v1/test-clocks/${clock.id}`, async route => {
    const response = await route.fetch()
    const json = await response.json() as TestClock
    await route.fulfill({ response, json: clockReads++ === 0 ? { ...json, status: 'advancing' } : json })
  })

  await page.goto('/test-clocks')
  const row = page.getByRole('row').filter({ hasText: clock.name })
  await expect(row.getByText('Advancing')).toBeVisible()

  // The clock page polls the clock and sees the advance finish.
  await row.getByRole('link', { name: clock.name }).click()
  await expect(page.getByRole('heading', { level: 1, name: clock.name })).toBeVisible()
  await expect(page.getByText('Ready', { exact: true })).toBeVisible()

  await page.getByRole('navigation', { name: 'Main navigation' }).getByRole('link', { name: 'Test clocks' }).click()
  await expect(row.getByText('Ready')).toBeVisible()
  await expect(row.getByText('Advancing')).toHaveCount(0)
})
