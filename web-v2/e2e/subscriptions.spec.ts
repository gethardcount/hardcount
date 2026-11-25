import { test, expect, type Page, type Response } from '@playwright/test'
import { api, uniq } from './fixtures/api'
import {
  createCustomer, createSubscription, subscribedCustomer,
  type Invoice, type Plan, type Subscription,
} from './fixtures/seed'

// Changing a subscription through the screen: its plan and its end. Each
// journey does what an operator does, then reads the API and checks that the
// page and the toast show what the API holds. One test per kind of change.

// The subscription fields these journeys read, beyond the shared fixture's.
interface Item {
  id: string
  plan_id: string
  pending_plan_id?: string
  pending_plan_effective_at?: string
}
interface Sub extends Subscription {
  items: Item[]
  current_billing_period_end?: string
  next_billing_at?: string
  cancel_at_period_end?: boolean
  cancel_effective_at?: string
  canceled_at?: string
}
interface ItemChange {
  item: Item
  proration?: unknown
}

function getSub(id: string) {
  return api<Sub>('GET', `/subscriptions/${id}`)
}

async function subscriptionInvoices(id: string): Promise<Invoice[]> {
  return (await api<{ data: Invoice[] }>('GET', `/invoices?subscription_id=${id}`)).data
}

// A monthly USD plan whose base fee is billed at the start of each period.
function planBilledInAdvance(baseCents: number): Promise<Plan> {
  const id = uniq()
  return api<Plan>('POST', '/plans', {
    code: `e2e-adv-${id}`,
    name: `E2E Advance ${id}`,
    currency: 'USD',
    billing_interval: 'monthly',
    base_amount_cents: baseCents,
    base_bill_timing: 'in_advance',
  })
}

// The workspace time zone: the dashboard prints every date in it.
async function workspaceZone(): Promise<string> {
  return (await api<{ timezone?: string }>('GET', '/settings')).timezone || 'UTC'
}

// A calendar date as the dashboard prints it: "Nov 18, 2026".
function day(iso: string, timeZone: string): string {
  return new Date(iso).toLocaleDateString('en-US', { year: 'numeric', month: 'short', day: 'numeric', timeZone })
}

// The next API call the page makes with this method to a path ending in `path`.
function apiCall(page: Page, method: string, path: RegExp): Promise<Response> {
  return page.waitForResponse(r => r.request().method() === method && path.test(new URL(r.url()).pathname))
}

// Toasts render outside the page's main region.
function toasts(page: Page) {
  return page.getByRole('region', { name: /^Notifications/ })
}

function itemsCard(page: Page) {
  return page.getByRole('main').locator('[data-slot="card"]').filter({ has: page.getByRole('columnheader', { name: 'Pending change' }) })
}

// The "Billing cycle" cell of the summary card and its value.
function summaryCell(page: Page, label: string) {
  return page.getByRole('main').getByText(label, { exact: true }).locator('..')
}

// The date the summary card gives for the next renewal: the API's next
// billing date.
function renewsOn(sub: Sub, zone: string): string {
  return day(sub.next_billing_at ?? sub.current_billing_period_end!, zone)
}

test('a change of plan at renewal charges nothing and shows as pending until canceled', async ({ page }) => {
  const small = await planBilledInAdvance(4900)
  const bigger = await planBilledInAdvance(9900)
  const customer = await createCustomer()
  const subscription = await createSubscription(customer.id, bigger.id)
  const zone = await workspaceZone()

  await page.goto(`/subscriptions/${subscription.id}`)
  const items = itemsCard(page)
  const dialog = page.getByRole('dialog', { name: 'Change plan' })

  // Change at renewal (Change now left unticked): nothing is charged; the
  // item shows the pending plan and its date until the change is canceled.
  await items.getByRole('button', { name: 'Change plan', exact: true }).click()
  await dialog.getByRole('button', { name: 'Select a plan...' }).click()
  await page.getByRole('option', { name: new RegExp(small.code) }).click()
  await expect(dialog.getByRole('checkbox', { name: /^Change now/ })).not.toBeChecked()
  const scheduling = apiCall(page, 'PATCH', /\/items\/[^/]+$/)
  await dialog.getByRole('button', { name: 'Change plan', exact: true }).click()
  const laterRes = await scheduling
  expect(laterRes.ok()).toBe(true)
  const later = (await laterRes.json()) as ItemChange
  expect(later.proration).toBeUndefined()
  expect(later.item.pending_plan_id).toBe(small.id)
  const effectiveOn = day(later.item.pending_plan_effective_at!, zone)
  await expect(toasts(page).getByText(`The new plan starts ${effectiveOn}.`, { exact: true })).toBeVisible()

  const pending = (await getSub(subscription.id)).items
  expect(pending.map(i => [i.plan_id, i.pending_plan_id])).toEqual([[bigger.id, small.id]])
  expect(day(pending[0].pending_plan_effective_at!, zone)).toBe(effectiveOn)
  expect(await subscriptionInvoices(subscription.id)).toHaveLength(1)
  const row = items.getByRole('row').filter({ hasText: bigger.name })
  await expect(row.getByText(`→ ${small.name}`, { exact: true })).toBeVisible()
  await expect(row).toContainText(effectiveOn)

  await row.getByRole('button', { name: 'Cancel', exact: true }).click()
  await expect(toasts(page).getByText('Pending plan change canceled', { exact: true })).toBeVisible()
  const cleared = (await getSub(subscription.id)).items
  expect(cleared.map(i => [i.plan_id, i.pending_plan_id ?? null, i.pending_plan_effective_at ?? null])).toEqual([[bigger.id, null, null]])
  await expect(row).not.toContainText(small.name)
  await expect(row.getByRole('button', { name: 'Cancel', exact: true })).toHaveCount(0)
})

test('cancel at period end shows its date and can be undone; cancel now needs CANCEL typed and reads "Ended"', async ({ page }) => {
  const { subscription } = await subscribedCustomer()
  const zone = await workspaceZone()
  const before = await getSub(subscription.id)

  await page.goto(`/subscriptions/${subscription.id}`)
  const main = page.getByRole('main')
  const billingCycle = summaryCell(page, 'Billing cycle')
  await expect(billingCycle).toContainText(`Renews ${renewsOn(before, zone)}`)
  const choice = page.getByRole('alertdialog', { name: 'Cancel subscription' })

  // At period end: the API keeps it active with a cancel date, and the page
  // names that date until Undo clears it.
  await main.getByRole('button', { name: 'Cancel', exact: true }).click()
  await expect(choice.getByRole('radio', { name: /^At period end/ })).toBeChecked()
  const scheduling = apiCall(page, 'POST', /\/schedule-cancel$/)
  await choice.getByRole('button', { name: 'Schedule cancellation' }).click()
  expect((await scheduling).ok()).toBe(true)
  const scheduled = await getSub(subscription.id)
  expect([scheduled.status, scheduled.cancel_at_period_end]).toEqual(['active', true])
  const cancelsOn = day(scheduled.cancel_effective_at!, zone)
  await expect(toasts(page).getByText(`Cancels at the end of the current period, ${cancelsOn}.`, { exact: true })).toBeVisible()
  await expect(main.getByText(`it doesn't renew on ${cancelsOn}.`, { exact: false })).toBeVisible()
  await expect(billingCycle).toContainText(`Cancels ${cancelsOn}`)

  const undoing = apiCall(page, 'DELETE', /\/scheduled-cancel$/)
  await main.getByRole('button', { name: 'Undo', exact: true }).click()
  expect((await undoing).ok()).toBe(true)
  await expect(toasts(page).getByText('Cancellation undone', { exact: true })).toBeVisible()
  const undone = await getSub(subscription.id)
  expect([undone.status, undone.cancel_at_period_end ?? false, undone.cancel_effective_at ?? null]).toEqual(['active', false, null])
  // The notice is gone (the Activity list keeps its "Cancellation scheduled"
  // entry, so look for the notice's own words and button).
  await expect(main.getByText(`it doesn't renew on ${cancelsOn}.`, { exact: false })).toHaveCount(0)
  await expect(main.getByRole('button', { name: 'Undo', exact: true })).toHaveCount(0)
  await expect(billingCycle).toContainText(`Renews ${renewsOn(undone, zone)}`)

  // Immediately: the typed confirm stays disabled until CANCEL is typed.
  await main.getByRole('button', { name: 'Cancel', exact: true }).click()
  await choice.getByRole('radio', { name: /^Immediately/ }).click()
  await choice.getByRole('button', { name: 'Cancel immediately…' }).click()
  const confirm = page.getByRole('alertdialog', { name: 'Cancel subscription now' })
  const cancelNow = confirm.getByRole('button', { name: 'Cancel subscription now' })
  await expect(cancelNow).toBeDisabled()
  await confirm.getByLabel('Type CANCEL to confirm').fill('CANCE')
  await expect(cancelNow).toBeDisabled()
  await confirm.getByLabel('Type CANCEL to confirm').fill('CANCEL')
  const canceling = apiCall(page, 'POST', /\/cancel$/)
  await cancelNow.click()
  expect((await canceling).ok()).toBe(true)
  await expect(toasts(page).getByText('Subscription canceled', { exact: true })).toBeVisible()

  const canceled = await getSub(subscription.id)
  expect(canceled.status).toBe('canceled')
  const endedOn = day(canceled.canceled_at!, zone)
  await expect(billingCycle).toContainText(`Canceled ${endedOn}`)
  await expect(main.getByText(/Renews/)).toHaveCount(0)

  await page.goto(`/subscriptions?search=${encodeURIComponent(subscription.code)}`)
  await expect(page.getByRole('row').filter({ hasText: subscription.display_name })).toContainText(`Ended ${endedOn}`)
})
