import { test, expect } from '@playwright/test'
import { createClock, type TestClock } from './fixtures/seed'

// A failed advance: the banner says the work it finished is kept, and the
// last-advance card shows those counts in plain words, with no error line of
// its own (the banner says the advance failed).
test('a failed advance keeps its finished work, counted in plain words on the card', async ({ page }) => {
  const clock = await createClock()
  await page.route(url => url.pathname === `/v1/test-clocks/${clock.id}`, async route => {
    const response = await route.fetch()
    const json = await response.json() as TestClock
    await route.fulfill({
      response,
      json: {
        ...json,
        status: 'internal_failure',
        last_failure_reason: "collect the clock's invoices: Invoice HC-1 was not charged",
        last_advance_summary: {
          advanced_from: json.frozen_time, advanced_to: json.frozen_time,
          invoices_generated: 2, trials_activated: 0, pauses_resumed: 0, thresholds_fired: 0, tax_retried: 0,
          charges_retried: 1, clawbacks_issued: 1, credits_expired: 0, dunning_advanced: 3,
        },
      },
    })
  })

  await page.goto(`/test-clocks/${clock.id}`)
  await expect(page.getByRole('heading', { level: 1, name: clock.name })).toBeVisible()
  await expect(page.getByText('Work finished before the failure is kept.')).toBeVisible()
  for (const label of ['Invoices generated', 'Charges attempted', 'Credit notes issued', 'Payment retry steps run']) {
    await expect(page.getByText(label, { exact: true })).toBeVisible()
  }
  await expect(page.getByText(/ended with an error/)).toHaveCount(0)
})
