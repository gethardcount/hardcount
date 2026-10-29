import { test, expect, type Page } from '@playwright/test'
import { api, uniq } from './fixtures/api'
import { createCustomer, invoicesFor, type Invoice } from './fixtures/seed'

// Payment retries (dunning) through the screen: closing a run by hand, and
// deleting a policy that has run. A run is made the way it happens for real:
// a customer with no card on file is billed, and Hardcount starts payment retries
// on the unpaid invoice under the customer's policy.

interface DunningRun { id: string; invoice_id: string; policy_id: string; state: string; resolution?: string }

function toasts(page: Page) {
  return page.getByRole('region', { name: /^Notifications/ })
}

// A policy of the test's own, turned on. It is created turned off and then
// turned on, because a new policy created turned on takes the default from
// the one the account was created with (ADR-123), and other specs change the
// default while they run.
async function ownPolicy(name: string): Promise<string> {
  const policy = { name, retry_days: [3, 4, 7], final_subscription_action: 'pause', final_invoice_action: 'none' }
  const created = await api<{ id: string }>('POST', '/dunning/policies', { ...policy, enabled: false })
  await api('PATCH', `/dunning/policies/${created.id}`, { ...policy, enabled: true })
  return created.id
}

// A customer with no card on the policy, subscribed to a plan billed in
// advance: the first invoice is due at once and goes into payment retries.
async function unpaidRunOn(policyId: string): Promise<{ customerId: string; invoice: Invoice; run: DunningRun }> {
  const id = uniq()
  const customer = await createCustomer()
  await api('PATCH', `/customers/${customer.id}`, { dunning_policy_id: policyId })
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
    return run?.policy_id
  }, { message: 'payment retries started on the unpaid invoice' }).toBe(policyId)
  return { customerId: customer.id, invoice, run: run! }
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
  const policyId = await ownPolicy(`E2E resolve ${uniq()}`)
  const { invoice, run } = await unpaidRunOn(policyId)

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

// #1215: a policy that has run keeps that history pointing at it, so it
// cannot be deleted. The API used to answer 500; now it says why, and the
// page shows it.
test('a dunning policy that has run cannot be deleted, and the page says why', async ({ page }) => {
  const name = `E2E used policy ${uniq()}`
  const policyId = await ownPolicy(name)
  const { customerId } = await unpaidRunOn(policyId)
  // Only the history is left: the customer is moved off the policy.
  await api('PATCH', `/customers/${customerId}`, { dunning_policy_id: '' })

  await page.goto('/dunning-policies')
  await page.getByRole('button', { name: `Actions for policy ${name}`, exact: true }).click()
  await page.getByRole('menuitem', { name: 'Delete', exact: true }).click()
  const confirm = page.getByRole('alertdialog')
  await expect(confirm).toContainText(name)
  const deleting = page.waitForResponse(r => r.request().method() === 'DELETE' && new URL(r.url()).pathname.endsWith(`/dunning/policies/${policyId}`))
  await confirm.getByRole('button', { name: 'Delete policy', exact: true }).click()
  const res = await deleting
  expect(res.status()).toBe(409)
  const message = ((await res.json()) as { error: { message: string } }).error.message
  expect(message).toMatch(/has run payment retries on 1 invoice/)
  await expect(toasts(page).getByText(message, { exact: false })).toBeVisible()
  // The policy is still there.
  const left = (await api<{ data: { id: string }[] }>('GET', '/dunning/policies')).data
  expect(left.map(p => p.id)).toContain(policyId)
  await expect(page.getByRole('button', { name: `Actions for policy ${name}`, exact: true })).toBeVisible()
})
