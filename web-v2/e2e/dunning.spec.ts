import { test, expect, type Page } from '@playwright/test'
import { api, uniq } from './fixtures/api'
import { createCustomer, invoicesFor, retriesOn, type Invoice } from './fixtures/seed'

// Payment retries (dunning) through the screen: closing a run by hand. A run
// is made the way it happens for real: a customer with no card on file is
// billed, and Hardcount starts payment retries on the unpaid invoice under
// the account's retry policy.

interface DunningRun { id: string; invoice_id: string; state: string; resolution?: string }

// A customer with no card, subscribed to a plan billed in advance: the first
// invoice is due at once and goes into payment retries.
async function unpaidRun(): Promise<{ invoice: Invoice; run: DunningRun }> {
  const id = uniq()
  await retriesOn()
  const customer = await createCustomer()
  const plan = await api<{ id: string }>('POST', '/plans', {
    code: `e2e-dun-${id}`, name: `E2E Dunning ${id}`, currency: 'USD',
    billing_interval: 'monthly', base_amount_cents: 4900, base_bill_timing: 'in_advance',
  })
  await api('POST', '/subscriptions', {
    code: `e2e-dun-${id}`, display_name: `E2E Dunning ${id}`, customer_id: customer.id,
    items: [{ plan_id: plan.id, quantity: 1 }], billing_time: 'anniversary',
  })
  const [invoice] = await invoicesFor(customer.id)
  expect(invoice, 'the subscription invoiced its first month').toBeTruthy()
  let run: DunningRun | undefined
  await expect.poll(async () => {
    run = (await api<{ data: DunningRun[] }>('GET', `/dunning/runs?invoice_id=${invoice.id}`)).data[0]
    return run?.state
  }, { message: 'payment retries started on the unpaid invoice' }).toBe('active')
  return { invoice, run: run! }
}

const RUNS_PAGE_SIZE = 25

// The Dunning page has no link to a single run, so the run's row is found as
// a person finds it: filter to active runs, then page through.
async function openRunRow(page: Page, invoice: Invoice) {
  await page.goto('/dunning')
  // The filtered list is drawn once its runs and their invoices are read.
  const filtered = page.waitForResponse(r => {
    const u = new URL(r.url())
    return u.pathname.endsWith('/dunning/runs') && u.searchParams.get('state') === 'active'
  })
  await page.getByRole('button', { name: 'Active', exact: true }).click()
  const runs = (await (await filtered).json()) as { data: DunningRun[] }
  const link = page.getByRole('link', { name: invoice.invoice_number, exact: true })
  const showing = page.getByText(/^Showing \d+–\d+ of \d+$/)
  if (runs.data.some(r => r.invoice_id === invoice.id)) {
    await expect(link).toBeVisible()
  } else {
    await expect(showing).toBeVisible()
  }
  for (let p = 1; !(await link.isVisible()); p++) {
    if (p >= 40) throw new Error(`run for ${invoice.invoice_number} not found in ${p} pages`)
    await page.getByLabel('Go to next page').click()
    await expect(page.getByText(new RegExp(`^Showing ${p * RUNS_PAGE_SIZE + 1}–`))).toBeVisible()
  }
  return page.getByRole('row').filter({ has: link })
}

// An active run is closed on its invoice, not in a second dialog here. The
// Dunning page's Resolve dialog was a weaker copy of the invoice's actions: it
// recorded a payment with no reference and voided or wrote off with no typed
// word (R-01; #1234 was its pre-selected "Payment recovered"). The row now
// opens the invoice, and recording an offline payment there, with its
// reference, closes the run as recovered.
test('an active run opens its invoice, and recording an offline payment there closes the run', async ({ page }) => {
  const { invoice, run } = await unpaidRun()

  const row = await openRunRow(page, invoice)
  await expect(row.getByRole('button', { name: 'Resolve', exact: true })).toHaveCount(0)
  await row.getByRole('link', { name: 'Open invoice', exact: true }).click()
  await expect(page).toHaveURL(new RegExp(`/invoices/${invoice.id}$`))
  await expect(page.getByRole('heading', { level: 1, name: invoice.invoice_number, exact: true })).toBeVisible()

  await page.getByRole('button', { name: 'More invoice actions' }).click()
  await page.getByRole('menuitem', { name: 'Record offline payment' }).click()
  const record = page.getByRole('dialog', { name: 'Record offline payment' })
  await record.getByLabel('Reference (optional)').fill(`Wire e2e-${uniq()}`)
  await record.getByRole('button', { name: 'Record payment' }).click()
  await expect(record).toBeHidden()

  const { invoice: paid } = await api<{ invoice: Invoice }>('GET', `/invoices/${invoice.id}`)
  expect(paid.status).toBe('paid')
  const after = (await api<{ data: DunningRun[] }>('GET', `/dunning/runs?invoice_id=${invoice.id}`)).data
  expect(after.map(r => [r.id, r.state, r.resolution])).toEqual([[run.id, 'resolved', 'payment_recovered']])
})
