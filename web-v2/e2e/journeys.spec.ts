import fs from 'node:fs'
import { test, expect, type Locator } from '@playwright/test'
import { env, seedFile } from './fixtures/env'
import { api, usd, uniq } from './fixtures/api'
import {
  createCustomer, createPlan, createSubscription, subscribedCustomer, invoicesFor,
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

test('a draft invoice reads as a draft and shows its memo', async ({ page }) => {
  const customer = await createCustomer()
  const memo = `E2E memo ${uniq()}`
  const invoice = await api<Invoice>('POST', '/invoices', {
    customer_id: customer.id,
    currency: 'USD',
    net_payment_term_days: 30,
    memo,
    line_items: [{ description: 'E2E setup fee', line_type: 'add_on', quantity: 1, unit_amount_cents: 1000 }],
  })
  expect(invoice.status).toBe('draft')

  await page.goto(`/invoices/${invoice.id}`)
  await expect(page.getByRole('heading', { level: 1, name: invoice.invoice_number, exact: true })).toBeVisible()
  // The API stamps provisional dates on a draft; the page must not show them
  // as if the invoice had been issued.
  await expect(page.getByText('Issued', { exact: true }).locator('..')).toContainText('Draft — not issued')
  await expect(page.getByText('Due', { exact: true }).locator('..')).toContainText('Net 30 from finalizing')
  await expect(page.getByText('Memo', { exact: true }).locator('..')).toContainText(memo)
})

test('the DRAFT mark fits inside the invoice at phone width', async ({ page }) => {
  const customer = await createCustomer()
  const invoice = await api<Invoice>('POST', '/invoices', {
    customer_id: customer.id,
    currency: 'USD',
    line_items: [{ description: 'E2E setup fee', line_type: 'add_on', quantity: 1, unit_amount_cents: 1000 }],
  })
  await page.setViewportSize({ width: 390, height: 844 })
  await page.goto(`/invoices/${invoice.id}`)
  const mark = page.getByTestId('draft-watermark')
  await expect(mark).toHaveText('DRAFT')
  // The card clips overflow, so measure the word itself, not its box.
  const fits = await mark.evaluate((el) => {
    const range = document.createRange()
    range.selectNodeContents(el)
    const word = range.getBoundingClientRect()
    const card = el.parentElement!.getBoundingClientRect()
    return word.left >= card.left && word.right <= card.right
  })
  expect(fits).toBe(true)
})

test('Home counts trialing subscriptions and compares failed payments with the previous period', async ({ page }) => {
  // A live (not test-clock) customer on a trial, so the trialing count the
  // Customers card reads from the server is at least 1.
  const plan = await createPlan()
  const customer = await createCustomer()
  const id = uniq()
  const sub = await api<Subscription>('POST', '/subscriptions', {
    code: `e2e-trial-${id}`,
    display_name: `E2E Trial ${id}`,
    customer_id: customer.id,
    items: [{ plan_id: plan.id, quantity: 1 }],
    trial_days: 14,
    start_now: true,
  })
  expect(sub.status).toBe('trialing')

  await page.goto('/')
  await expect(page.getByRole('heading', { level: 1, name: 'Home', exact: true })).toBeVisible()
  // The definition is on the card as text, not only in a hover tooltip.
  const definition = page.getByText(/^Customers with an active or trialing subscription/)
  await expect(definition).toBeVisible()
  const customers = definition.locator('..')
  await expect(customers).toContainText(/\d+ active subscriptions? · [1-9]\d* trialing subscriptions?/)
  await expect(customers).not.toContainText(/\d+ active · /)
  const failed = page.getByText('Failed payments', { exact: true }).locator('..')
  await expect(failed).toContainText(/\d+ in the previous 30 days/)
})

test('the Usage page shows a Maximum meter as its peak per customer, not the sum', async ({ page }) => {
  const id = uniq()
  const key = `e2e_storage_${id}`
  const meter = await api<{ id: string; name: string }>('POST', '/meters', {
    key, name: `E2E Storage across every region and every storage tier ${id}`, unit: 'GB', aggregation: 'max',
  })
  const a = await createCustomer()
  const b = await createCustomer()
  for (const [customer, quantity] of [[a, '140'], [b, '210'], [b, '120']] as const) {
    await api('POST', '/usage-events', { external_customer_id: customer.external_id, event_name: key, quantity, idempotency_key: `e2e_${uniq()}` })
  }

  await page.goto(`/usage?meter=${meter.id}`)
  await expect(page.getByRole('heading', { level: 1, name: 'Usage Events', exact: true })).toBeVisible()
  const row = page.getByRole('listitem').filter({ has: page.getByRole('link', { name: meter.name, exact: true }) })
  // 140 (A's peak) + 210 (B's peak), never 140 + 210 + 120 = 470.
  await expect(row).toContainText('350 GB')
  await expect(row).toContainText('Peak reading per customer, added across 2 customers')
  await expect(row).not.toContainText('470')
  // A long meter name is shown in full (it wraps), never cut off.
  const name = row.getByRole('link', { name: meter.name, exact: true })
  expect(await name.evaluate((el) => el.scrollWidth <= el.clientWidth)).toBe(true)
})

test('the next-invoice preview prorates a mid-month calendar start', async ({ page }) => {
  // A clock frozen on Jan 13 and a calendar-billed subscription started then:
  // its first period is Jan 13–31, 19 of January's 31 days.
  const plan = await createPlan()
  const clock = await api<{ id: string }>('POST', '/test-clocks', { name: `E2E Clock ${uniq()}`, frozen_time: '2026-01-13T12:00:00Z' })
  const customer = await createCustomer(clock.id)
  const id = uniq()
  const sub = await api<Subscription>('POST', '/subscriptions', {
    code: `e2e-cal-${id}`,
    display_name: `E2E Calendar ${id}`,
    customer_id: customer.id,
    items: [{ plan_id: plan.id, quantity: 1 }],
    billing_time: 'calendar',
    start_now: true,
  })
  const preview = await api<{ lines: { description: string; amount_cents: number }[] }>('GET', `/billing/preview/${sub.id}`)
  expect(preview.lines).toHaveLength(1)
  const [base] = preview.lines
  expect(base.description).toBe(`${plan.name} - base fee (qty 1, prorated 19/31 days)`)

  await page.goto(`/subscriptions/${sub.id}`)
  const row = page.getByRole('row').filter({ hasText: base.description })
  await expect(row).toBeVisible()
  await expect(row).toContainText(usd(base.amount_cents))
})

test('the next-invoice preview says why a trial set to cancel has no invoice', async ({ page }) => {
  // A trial canceled at its end issues no invoice, so the preview has no
  // lines. The card used to show an empty table with no words.
  const plan = await createPlan()
  const customer = await createCustomer()
  const id = uniq()
  const sub = await api<Subscription>('POST', '/subscriptions', {
    code: `e2e-trial-${id}`,
    display_name: `E2E Trial ${id}`,
    customer_id: customer.id,
    items: [{ plan_id: plan.id, quantity: 1 }],
    trial_days: 14,
  })
  await api('POST', `/subscriptions/${sub.id}/schedule-cancel`, { at_period_end: true })
  const preview = await api<{ lines: unknown[]; warnings: string[] }>('GET', `/billing/preview/${sub.id}`)
  expect(preview.lines).toHaveLength(0)
  expect(preview.warnings).toContain('A cancel is scheduled for the end of the trial, so no invoice will be issued.')

  await page.goto(`/subscriptions/${sub.id}`)
  await expect(page.getByText('A cancel is scheduled for the end of the trial, so no invoice will be issued.')).toBeVisible()
  await expect(page.getByRole('columnheader', { name: 'Description' })).toHaveCount(0)
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

test("a EUR-plan customer's credit balance reads in euros", async ({ page }) => {
  // No billing profile and no invoice yet: the live plan sets the currency.
  // The page used to fall back to the workspace's USD and read $0.00.
  const plan = await createPlan('EUR')
  const customer = await createCustomer()
  await createSubscription(customer.id, plan.id)

  // Hold the plan list back: until the page knows the currency it must not
  // show one, and New invoice must wait, or the dialog opens in USD.
  let release!: () => void
  const held = new Promise<void>(r => { release = r })
  await page.route(/\/v1\/plans(\?|$)/, async route => { await held; await route.continue() })
  await page.goto(`/customers/${customer.id}`)
  const balance = page.getByRole('link', { name: /Credit Balance/ })
  const newInvoice = page.getByRole('button', { name: 'New invoice' })
  await expect(newInvoice).toBeDisabled()
  await expect(balance).toContainText('—')
  await expect(balance).not.toContainText('$')
  release()

  await expect(balance).toContainText('€0.00')
  await newInvoice.click()
  await expect(page.getByRole('dialog').getByLabel('Currency')).toContainText('EUR')
})

test('Margin shows no margin when no provider cost is known', async ({ page }) => {
  // The card used to read "Provider cost $0.0000" here instead of a dash.
  // The "not configured" sentence needs a workspace with no provider cost
  // rate at all; check that first so a rate left in the database fails here
  // with a reason, not later on a missing sentence.
  const rates = await api<{ data: unknown[] }>('GET', '/provider-costs')
  expect(rates.data, 'this test needs a workspace with no provider cost rates; delete them or use a fresh e2e database').toHaveLength(0)
  const { customer } = await subscribedCustomer()
  await page.goto(`/customers/${customer.id}`)
  const card = page.locator('[data-slot="card"]').filter({ has: page.getByText('Usage billing vs your provider costs') })
  await expect(card.getByText('No provider costs configured, so there is no margin to show.')).toBeVisible()
  await expect(card.getByRole('link', { name: 'Set up provider costs' })).toHaveAttribute('href', '/provider-costs')
  await expect(card.getByText('Provider cost', { exact: true }).locator('..')).toContainText('—')
  await expect(card.getByText('Margin', { exact: true }).locator('..').last()).not.toContainText('%')
})
