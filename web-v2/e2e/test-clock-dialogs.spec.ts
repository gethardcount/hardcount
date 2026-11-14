import { test, expect } from '@playwright/test'
import { createClock, createCustomer } from './fixtures/seed'

// The clock page counts the customers on the clock; each customer's page
// lists its subscriptions. The advance dialog says how many customers move,
// and the delete dialog says their subscriptions, invoices, usage and credits
// go with the clock, with no subscription count of its own.
test('the advance and delete dialogs count the customers on the clock', async ({ page }) => {
  const clock = await createClock()
  await createCustomer(clock.id)

  await page.goto(`/test-clocks/${clock.id}`)
  await expect(page.getByRole('heading', { level: 1, name: clock.name })).toBeVisible()
  await expect(page.getByText('Subscriptions on this clock')).toHaveCount(0)

  await page.getByRole('button', { name: 'Advance', exact: true }).click()
  const advance = page.getByRole('dialog')
  await expect(advance).toContainText('Moves time forward for 1 customer on this clock')
  await expect(advance).not.toContainText('subscription')
  await page.keyboard.press('Escape')

  await page.getByRole('button', { name: 'Delete test clock' }).click()
  const del = page.getByRole('alertdialog')
  await expect(del).toContainText('its customer, with their subscriptions, invoices, usage and credits')
  await del.getByRole('button', { name: 'Cancel' }).click()
})
