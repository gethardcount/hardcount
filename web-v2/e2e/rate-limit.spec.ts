import { test, expect } from '@playwright/test'
import { createCustomer } from './fixtures/seed'

// A page whose load is refused with 429 (Too many requests) shows its error
// state at once, says it will try again, and loads by itself once
// Retry-After has passed. Before, React Query retried the 429 like any
// failure and the page sat on its skeleton. CI's API is not rate-limited
// (no Redis), so the browser stubs the first answer.
test('a 429 on a page load shows "trying again", then the page loads', async ({ page }) => {
  const customer = await createCustomer()
  const path = `/v1/customers/${customer.id}`
  let refused = 0
  await page.route(url => url.pathname === path, async route => {
    if (route.request().method() === 'GET' && refused === 0) {
      refused++
      await route.fulfill({
        status: 429,
        headers: { 'Retry-After': '1' },
        json: { error: { type: 'rate_limit_error', code: 'rate_limited', message: 'too many requests' } },
      })
      return
    }
    await route.fallback()
  })

  await page.goto(`/customers/${customer.id}`)
  const alert = page.getByRole('alert').filter({ hasText: "Couldn't load this customer" })
  await expect(alert).toContainText('Too many requests. Trying again in a moment.')

  // No click: the page loads again by itself after Retry-After.
  await expect(page.getByRole('heading', { level: 1, name: customer.display_name, exact: true })).toBeVisible({ timeout: 15_000 })
  await expect(alert).toHaveCount(0)
  expect(refused).toBe(1)
})
