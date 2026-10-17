import fs from 'node:fs'
import { test, expect, type Page } from '@playwright/test'
import { seedFile } from './fixtures/env'
import type { Seed } from './fixtures/seed'

// Every page, at a desktop and a phone width, must load with no console
// errors, no failed API calls, no sideways page scroll, and its heading shown.
// Keep this list in step with the routes in src/main.tsx.

interface Route {
  name: string
  path: (s: Seed) => string
  heading: (s: Seed) => string
  public?: boolean
}

const routes: Route[] = [
  { name: 'home', path: () => '/', heading: () => 'Home' },
  { name: 'recipes', path: () => '/recipes', heading: () => 'Recipes' },
  { name: 'customers', path: () => '/customers', heading: () => 'Customers' },
  { name: 'customer detail', path: s => `/customers/${s.customer.id}`, heading: s => s.customer.display_name },
  { name: 'pricing', path: () => '/pricing', heading: () => 'Pricing' },
  { name: 'plan detail', path: s => `/plans/${s.plan.id}`, heading: s => s.plan.name },
  { name: 'meter detail', path: s => `/meters/${s.meter.id}`, heading: s => s.meter.name },
  { name: 'subscriptions', path: () => '/subscriptions', heading: () => 'Subscriptions' },
  { name: 'subscription detail', path: s => `/subscriptions/${s.subscription.id}`, heading: s => s.subscription.display_name },
  { name: 'invoices', path: () => '/invoices', heading: () => 'Invoices' },
  { name: 'invoice detail', path: s => `/invoices/${s.invoice.id}`, heading: s => s.invoice.invoice_number },
  { name: 'credit notes', path: () => '/credit-notes', heading: () => 'Credit notes' },
  { name: 'usage', path: () => '/usage', heading: () => 'Usage' },
  { name: 'credits', path: () => '/credits', heading: () => 'Credits' },
  { name: 'customer credits', path: s => `/credits?customer=${s.customer.id}`, heading: s => s.customer.display_name },
  { name: 'provider costs', path: () => '/provider-costs', heading: () => 'Provider costs' },
  { name: 'dunning', path: () => '/dunning', heading: () => 'Dunning' },
  { name: 'dunning policies', path: () => '/dunning-policies', heading: () => 'Dunning policies' },
  { name: 'webhooks', path: () => '/webhooks', heading: () => 'Webhooks' },
  { name: 'webhook events', path: () => '/webhooks/events', heading: () => 'Webhooks' },
  { name: 'webhook endpoint detail', path: s => `/webhooks/endpoints/${s.webhookEndpointId}`, heading: () => 'Webhooks' },
  { name: 'api keys', path: () => '/api-keys', heading: () => 'API keys' },
  { name: 'test clocks', path: () => '/test-clocks', heading: () => 'Test Clocks' },
  { name: 'test clock detail', path: s => `/test-clocks/${s.clock.id}`, heading: s => s.clock.name },
  { name: 'audit log', path: () => '/audit-log', heading: () => 'Audit log' },
  { name: 'settings', path: () => '/settings', heading: () => 'Settings' },
  { name: 'settings email', path: () => '/settings?tab=email', heading: () => 'Settings' },
  { name: 'hosted invoice (public)', path: s => `/invoice/${s.invoice.public_token}`, heading: s => `Invoice ${s.invoice.invoice_number}`, public: true },
]

// API calls that may answer 404 on a healthy page, because 404 is how the API
// says "there is none yet". Each entry names the page state it belongs to.
const expected404s: RegExp[] = [
  // A customer with no billing profile yet (the page offers to add one).
  /^\/v1\/customers\/[^/]+\/billing-profile$/,
]

const viewports = [
  { name: 'desktop', size: { width: 1440, height: 900 } },
  { name: 'phone', size: { width: 390, height: 844 } },
]

function readSeed(): Seed {
  return JSON.parse(fs.readFileSync(seedFile, 'utf8')) as Seed
}

// Collects every problem the page reports while it loads.
function watchForProblems(page: Page): string[] {
  const problems: string[] = []
  const isExpected404 = (url: string) => expected404s.some(re => re.test(new URL(url).pathname))
  page.on('console', msg => {
    if (msg.type() !== 'error') return
    // The browser logs every failed request to the console as well; those
    // are judged by the response check below.
    if (msg.text().startsWith('Failed to load resource') && isExpected404(msg.location().url)) return
    problems.push(`console error: ${msg.text()}`)
  })
  page.on('pageerror', err => problems.push(`uncaught exception: ${err.message}`))
  page.on('response', res => {
    const url = new URL(res.url())
    if (!url.pathname.startsWith('/v1/') || res.status() < 400) return
    if (res.status() === 404 && isExpected404(res.url())) return
    problems.push(`${res.request().method()} ${url.pathname}${url.search} returned ${res.status()}`)
  })
  page.on('requestfailed', req => {
    // A request the app cancels itself (navigating away, a superseded query)
    // is not a failure.
    if (req.failure()?.errorText === 'net::ERR_ABORTED') return
    problems.push(`${req.method()} ${req.url()} failed: ${req.failure()?.errorText}`)
  })
  return problems
}

for (const vp of viewports) {
  test.describe(`${vp.name} ${vp.size.width}px`, () => {
    test.use({ viewport: vp.size })

    for (const route of routes) {
      test(route.name, async ({ page }) => {
        const seed = readSeed()
        if (route.public) await page.context().clearCookies()
        const problems = watchForProblems(page)

        await page.goto(route.path(seed))
        await expect(page.getByRole('heading', { level: 1, name: route.heading(seed), exact: true })).toBeVisible()
        // Let the page's own data requests finish before judging it.
        await page.waitForLoadState('networkidle')

        // Measured on the page as rendered: nothing is injected first. The
        // dashboard scrolls inside its <main> pane, not the document, so a
        // too-wide page shows up there; the public pages have no <main> pane
        // and scroll the document.
        const overflow = await page.evaluate(() =>
          [document.documentElement, document.querySelector('main')]
            .filter(el => el !== null)
            .map(el => ({ element: el.tagName.toLowerCase(), scrollWidth: el.scrollWidth, clientWidth: el.clientWidth })),
        )
        for (const o of overflow) {
          expect(o.scrollWidth, `<${o.element}> scrolls sideways: content is wider than the window`).toBeLessThanOrEqual(o.clientWidth)
        }
        expect(problems, 'problems reported while the page loaded').toEqual([])
      })
    }
  })
}
