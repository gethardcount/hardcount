import { test, expect, type Page } from '@playwright/test'
import { api, uniq } from './fixtures/api'
import { createCustomer, invoicesFor, type Invoice } from './fixtures/seed'

// Payment retries (dunning) through the screen: closing a run by hand, and
// deleting a policy that has run. A run is made the way it happens for real:
// a customer with no card on file is billed, and Hardcount starts payment retries
// on the unpaid invoice under the customer's policy.

interface DunningRun { id: string; invoice_id: string; policy_id: string; state: string }

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

// #1234: the Resolve dialog used to open with "Payment recovered" chosen, so
// Resolve with no other click recorded a payment and marked the invoice paid.
test('resolve a payment-retry run: nothing is chosen at first, and Resolve waits for a choice', async ({ page }) => {
  const policyId = await ownPolicy(`E2E resolve ${uniq()}`)
  const { invoice, run } = await unpaidRunOn(policyId)

  const row = await openRunRow(page, invoice)
  await row.getByRole('button', { name: 'Resolve', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Resolve Dunning Run' })
  const recovered = dialog.getByRole('button', { name: /^Payment recovered/ })
  const voidIt = dialog.getByRole('button', { name: /^Void invoice/ })
  const writeOff = dialog.getByRole('button', { name: /^Write off invoice/ })
  const resolve = dialog.getByRole('button', { name: 'Resolve', exact: true })

  for (const choice of [recovered, voidIt, writeOff]) {
    await expect(choice).toHaveAttribute('aria-pressed', 'false')
  }
  await expect(resolve).toBeDisabled()
  await expect(recovered).toContainText('Records the payment, marks the invoice paid and stops the retries.')

  // A choice enables Resolve. Cancel leaves the run and the invoice as they were.
  await voidIt.click()
  await expect(voidIt).toHaveAttribute('aria-pressed', 'true')
  await expect(recovered).toHaveAttribute('aria-pressed', 'false')
  await expect(resolve).toBeEnabled()
  await dialog.getByRole('button', { name: 'Cancel', exact: true }).click()
  await expect(dialog).toBeHidden()

  const after = (await api<{ data: DunningRun[] }>('GET', `/dunning/runs?invoice_id=${invoice.id}`)).data
  expect(after.map(r => [r.id, r.state])).toEqual([[run.id, 'active']])
  const { invoice: still } = await api<{ invoice: Invoice }>('GET', `/invoices/${invoice.id}`)
  expect(still.status).toBe('finalized')
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
