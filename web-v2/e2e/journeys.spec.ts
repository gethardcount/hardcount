import fs from 'node:fs'
import { test, expect, type Locator } from '@playwright/test'
import { env, seedFile } from './fixtures/env'
import { api, usd, uniq } from './fixtures/api'
import {
  createCustomer, createPlan, subscribedCustomer, invoicesFor,
  type Customer, type Invoice, type Seed, type Subscription,
} from './fixtures/seed'

// The few things an operator does most. Each test seeds what it needs through
// the API, does one job through the screen, and checks that the screen shows
// what the API holds. The arithmetic behind the numbers is tested in Go.

test.describe('signed out', () => {
  test.use({ storageState: { cookies: [], origins: [] } })

  test('sign in', async ({ page }) => {
    await page.goto('/customers')
    await expect(page).toHaveURL(/\/login$/)
    await page.getByLabel('Email').fill(env.email)
    await page.getByLabel('Password', { exact: true }).fill(env.password)
    await page.getByRole('button', { name: 'Sign In' }).click()
    // Sign-in returns the operator to the page they asked for.
    await expect(page.getByRole('heading', { level: 1, name: 'Customers', exact: true })).toBeVisible()
  })

  test('hosted invoice page shows the invoice to the customer', async ({ page }) => {
    const { invoice } = JSON.parse(fs.readFileSync(seedFile, 'utf8')) as Seed
    await page.goto(`/invoice/${invoice.public_token}`)
    await expect(page.getByText(invoice.invoice_number)).toBeVisible()
    await expect(page.getByText('Total', { exact: true }).locator('..')).toContainText(usd(invoice.total_amount_cents))
  })

  test('sign-in follows dark mode; the hosted invoice stays light', async ({ page }) => {
    const { invoice } = JSON.parse(fs.readFileSync(seedFile, 'utf8')) as Seed
    await page.addInitScript(() => localStorage.setItem('velox-theme', 'dark'))
    await page.goto('/login')
    await expect(page.locator('html')).toHaveClass(/\bdark\b/)
    await page.goto(`/invoice/${invoice.public_token}`)
    await expect(page.getByText(invoice.invoice_number)).toBeVisible()
    await expect(page.locator('html')).not.toHaveClass(/\bdark\b/)
  })
})

test('create a customer', async ({ page }) => {
  const id = uniq()
  const name = `UI Customer ${id}`
  await page.goto('/customers')
  await page.getByRole('button', { name: 'Add Customer' }).click()
  const dialog = page.getByRole('dialog', { name: 'Create Customer' })
  await dialog.getByLabel('Display Name').fill(name)
  await dialog.getByLabel('External ID').fill(`ui_${id}`)
  await dialog.getByLabel('Email').fill(`ui-${id}@example.com`)
  await dialog.getByRole('button', { name: 'Create Customer' }).click()
  await expect(dialog).toBeHidden()

  const found = await api<{ data: Customer[] }>('GET', `/customers?search=ui_${id}`)
  expect(found.data.map(c => c.display_name)).toEqual([name])
  await page.getByPlaceholder('Search by name, email, or ID...').fill(`ui_${id}`)
  await expect(page.getByRole('row').filter({ hasText: name })).toBeVisible()
})

test('create a subscription for a customer', async ({ page }) => {
  const plan = await createPlan()
  const customer = await createCustomer()
  const id = uniq()
  await page.goto(`/customers/${customer.id}`)
  await page.getByRole('button', { name: 'Create Subscription' }).click()
  const dialog = page.getByRole('dialog', { name: 'Create Subscription' })
  await dialog.getByLabel('Display Name').fill(`UI Subscription ${id}`)
  await dialog.getByLabel('Code').fill(`ui-sub-${id}`)
  await dialog.getByLabel('Plan', { exact: true }).selectOption({ label: plan.name })
  await dialog.getByRole('button', { name: 'Create Subscription' }).click()
  await expect(dialog).toBeHidden()

  const subs = await api<{ data: Subscription[] }>('GET', `/subscriptions?customer_id=${customer.id}`)
  expect(subs.data.map(s => [s.code, s.status])).toEqual([[`ui-sub-${id}`, 'active']])
  await expect(page.getByRole('link', { name: `UI Subscription ${id}` })).toBeVisible()
})

test('advance a test clock to the renewal and see the invoice', async ({ page }) => {
  const { clock, customer } = await subscribedCustomer()
  await page.goto(`/test-clocks/${clock.id}`)
  await page.getByRole('button', { name: 'Advance', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Advance clock' })
  await dialog.getByRole('button', { name: '+1mo' }).click()
  await dialog.getByRole('button', { name: 'Advance clock' }).click()
  await expect(dialog).toBeHidden()
  // The page shows the outcome once the billing catchup has finished.
  await expect(page.getByText('Last advance results')).toBeVisible({ timeout: 30_000 })

  const invoices = await invoicesFor(customer.id)
  expect(invoices).toHaveLength(1)
  const { invoice } = await api<{ invoice: Invoice }>('GET', `/invoices/${invoices[0].id}`)

  await page.goto('/invoices')
  await page.getByPlaceholder('Search by invoice number...').fill(invoice.invoice_number)
  // The list names the state as the invoice page does: "Open", not the raw
  // "finalized".
  expect(invoice.status).toBe('finalized')
  const row = page.getByRole('row').filter({ hasText: invoice.invoice_number })
  await expect(row.getByText('Open', { exact: true })).toBeVisible()
  await expect(row).not.toContainText('finalized')
  await page.getByRole('link', { name: invoice.invoice_number, exact: true }).click()
  await expect(page.getByRole('heading', { level: 1, name: invoice.invoice_number, exact: true })).toBeVisible()
  await expect(page.getByText('Subtotal', { exact: true }).locator('..')).toContainText(usd(invoice.subtotal_cents))
  await expect(page.getByText('Amount Due', { exact: true }).locator('..')).toContainText(usd(invoice.amount_due_cents))
})

test('a dialog taller than the window keeps its actions in view', async ({ page }) => {
  const customer = await createCustomer()
  await page.setViewportSize({ width: 1440, height: 520 })
  await page.goto(`/customers/${customer.id}`)
  await page.getByRole('button', { name: 'New invoice' }).click()
  const dialog = page.getByRole('dialog', { name: 'New invoice' })
  await expect(dialog).toBeVisible()
  // The dialog is capped at the window and scrolls inside itself...
  expect(await dialog.evaluate(el => el.scrollHeight > el.clientHeight)).toBe(true)
  // ...while Cancel and Save & send stay on screen.
  await expect(dialog.getByRole('button', { name: 'Save & send' })).toBeInViewport()
  await expect(dialog.getByRole('button', { name: 'Cancel' })).toBeInViewport()
})

// True when the element's box lies inside the box of `outer` (by default the
// window). A dialog whose content is wider than the dialog pushes its buttons
// past the right edge, where no one can reach them.
async function boxInside(el: Locator, outer?: Locator) {
  const b = await el.boundingBox()
  const o = outer ? await outer.boundingBox() : { x: 0, y: 0, ...el.page().viewportSize()! }
  if (!b || !o) return false
  return b.x >= o.x - 0.5 && b.y >= o.y - 0.5 && b.x + b.width <= o.x + o.width + 0.5 && b.y + b.height <= o.y + o.height + 0.5
}

for (const width of [1440, 390]) {
  test(`change plan at ${width}px: a long plan name keeps the button in the dialog`, async ({ page }) => {
    const { subscription } = await subscribedCustomer()
    const other = await createPlan()
    await page.setViewportSize({ width, height: 844 })
    await page.goto(`/subscriptions/${subscription.id}`)
    await page.getByRole('button', { name: 'Change Plan', exact: true }).click()
    const dialog = page.getByRole('dialog', { name: 'Change Plan' })
    await dialog.getByRole('button', { name: 'Select a plan...' }).click()
    await page.getByRole('option', { name: new RegExp(other.code) }).click()
    await expect(dialog.getByRole('button', { name: new RegExp(other.code) })).toBeVisible()
    // Nothing in the dialog is wider than the dialog, and both actions are
    // fully inside it and on screen.
    expect(await dialog.evaluate(el => el.scrollWidth <= el.clientWidth)).toBe(true)
    for (const name of ['Change Plan', 'Cancel']) {
      const button = dialog.getByRole('button', { name, exact: true })
      expect(await boxInside(button, dialog)).toBe(true)
      expect(await boxInside(button)).toBe(true)
    }
  })
}

test('new invoice at phone width fits the dialog', async ({ page }) => {
  const customer = await createCustomer()
  await page.setViewportSize({ width: 390, height: 844 })
  await page.goto(`/customers/${customer.id}`)
  await page.getByRole('button', { name: 'New invoice' }).click()
  const dialog = page.getByRole('dialog', { name: 'New invoice' })
  await expect(dialog).toBeVisible()
  expect(await boxInside(dialog)).toBe(true)
  // No content wider than the dialog: no sideways scroll inside it, and every
  // field and button sits within its left and right edges (a clipped field
  // would not show up as scroll).
  expect(await dialog.evaluate(el => el.scrollWidth <= el.clientWidth)).toBe(true)
  const outside = await dialog.evaluate(el => {
    const d = el.getBoundingClientRect()
    return [...el.querySelectorAll('input:not([aria-hidden="true"]), button, textarea, [role="combobox"]')]
      .filter(c => { const r = c.getBoundingClientRect(); return r.width > 0 && (r.left < d.left - 0.5 || r.right > d.right + 0.5) })
      .map(c => c.getAttribute('aria-label') || c.getAttribute('placeholder') || c.textContent)
  })
  expect(outside).toEqual([])
  // ...and the actions are inside it and on screen.
  const send = dialog.getByRole('button', { name: 'Save & send' })
  await expect(send).toBeInViewport({ ratio: 1 })
  expect(await boxInside(send, dialog)).toBe(true)
})

test('Ctrl+K opens any page, including the ones it used to miss', async ({ page }) => {
  await page.goto('/customers')
  await expect(page.getByRole('heading', { level: 1, name: 'Customers', exact: true })).toBeVisible()
  await page.keyboard.press('Control+k')
  const palette = page.getByRole('dialog')
  await palette.getByPlaceholder('Search customers, invoices, subscriptions...').fill('provider')
  await palette.getByRole('option', { name: /^Provider costs/ }).click()
  await expect(page.getByRole('heading', { level: 1, name: 'Provider costs', exact: true })).toBeVisible()
})

test('Ctrl+K names an invoice the way the invoice page does', async ({ page }) => {
  const { invoice } = JSON.parse(fs.readFileSync(seedFile, 'utf8')) as Seed
  expect(invoice.status).toBe('finalized')
  await page.goto('/customers')
  await expect(page.getByRole('heading', { level: 1, name: 'Customers', exact: true })).toBeVisible()
  await page.keyboard.press('Control+k')
  const palette = page.getByRole('dialog')
  await palette.getByPlaceholder('Search customers, invoices, subscriptions...').fill(invoice.invoice_number)
  const option = palette.getByRole('option', { name: new RegExp(invoice.invoice_number) })
  await expect(option).toContainText(`Open · ${usd(invoice.amount_due_cents)} due`)
  await expect(option).not.toContainText('finalized')
})

test('Escape closes the mobile menu', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await page.goto('/customers')
  await page.getByRole('button', { name: 'Open menu' }).click()
  const close = page.getByRole('button', { name: 'Close menu' })
  await expect(close).toBeInViewport()
  // A phone has no keyboard shortcut to show.
  const search = page.getByRole('button', { name: /^Search/ }).filter({ visible: true })
  await expect(search).toBeVisible()
  await expect(search.locator('kbd')).toBeHidden()
  await page.keyboard.press('Escape')
  await expect(close).not.toBeInViewport()
})

test('grant credits and see the balance', async ({ page }) => {
  const customer = await createCustomer()
  await page.goto(`/customers/${customer.id}`)
  await page.getByRole('link', { name: /Credit Balance/ }).click()
  await expect(page.getByRole('heading', { level: 1, name: customer.display_name, exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'Grant', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Grant Credits' })
  await dialog.getByLabel(/^Amount/).fill('25.00')
  await dialog.getByLabel('Description').fill('E2E goodwill credit')
  await dialog.getByRole('button', { name: 'Grant Credits' }).click()
  await page.getByRole('alertdialog', { name: 'Confirm Credit Grant' }).getByRole('button', { name: 'Grant Credits' }).click()
  await expect(page.getByRole('row').filter({ hasText: 'E2E goodwill credit' })).toBeVisible()

  const balance = await api<{ balance_cents: number }>('GET', `/credits/balance/${customer.id}`)
  expect(balance.balance_cents).toBe(2500)
  await expect(page.getByText('Credit Balance', { exact: true }).locator('..')).toContainText(usd(balance.balance_cents))
})
