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

  // A clock frozen 60 days ago: from real time the invoice would read
  // Past due, and from the clock's time it reads Due in.
  const frozen = new Date(Date.now() - 60 * 86_400_000).toISOString().replace(/\.\d+Z$/, 'Z')
  const clock = await api<TestClock>('POST', '/test-clocks', { name: `E2E Clock ${uniq()}`, frozen_time: frozen })
  const invoice = await openInvoiceOn(clock.id)

  await invoiceRow(page, invoice)
  await openInvoice(page, invoice)
  await expect(page.getByRole('link', { name: 'View clock' })).toHaveAttribute('href', `/test-clocks/${clock.id}`)
  await expect(page.getByText('Simulated time', { exact: true })).toBeVisible()
  await expect(page.getByText('Test clock deleted.')).toHaveCount(0)
  await expect(page.getByText(/^Due in \d+d$/).first()).toBeVisible()
  await expect(page.getByText(/^Past due \d+d$/)).toHaveCount(0)
})
