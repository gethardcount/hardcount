import fs from 'node:fs'
import { test, expect, type Page } from '@playwright/test'
import { seedFile } from './fixtures/env'
import type { Seed } from './fixtures/seed'

// Every page, at a desktop and a phone width, must load with no console
// errors, no failed API calls, no sideways page scroll, and its heading shown.
// Keep this list in step with the routes in src/main.tsx, public ones too.

// A page shows it has rendered with its h1, or, on the sign-in pages, which
// show the logo instead of a heading, with a line of its text. A public page
// is opened signed out.
type Route = {
  name: string
  path: (s: Seed) => string
  public?: boolean
} & ({ heading: (s: Seed) => string } | { text: string })

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
  { name: 'test clocks', path: () => '/test-clocks', heading: () => 'Test clocks' },
  { name: 'test clock detail', path: s => `/test-clocks/${s.clock.id}`, heading: s => s.clock.name },
  { name: 'audit log', path: () => '/audit-log', heading: () => 'Audit log' },
  { name: 'settings', path: () => '/settings', heading: () => 'Settings' },
  { name: 'settings email', path: () => '/settings?tab=email', heading: () => 'Settings' },
  { name: 'hosted invoice (public)', path: s => `/invoice/${s.invoice.public_token}`, heading: s => `Invoice ${s.invoice.invoice_number}`, public: true },
  { name: 'sign in (public)', path: () => '/login', text: 'Sign in to the dashboard', public: true },
  { name: 'forgot password (public)', path: () => '/forgot-password', text: 'Reset your password', public: true },
  // The emailed links, opened with no code: each says the link is no good.
  { name: 'reset password, no code (public)', path: () => '/reset-password', text: 'This reset link is no longer valid.', public: true },
  { name: 'accept invite, no code (public)', path: () => '/accept-invite', text: 'This invitation link is no longer valid.', public: true },
  { name: 'update payment, no code (public)', path: () => '/update-payment', heading: () => 'Secure payment update', public: true },
  // Where Stripe Checkout sends a customer back after adding a card, or not.
  { name: 'payment method added (public)', path: () => '/payment-method-added?status=success', heading: () => 'Payment method added', public: true },
  { name: 'payment method setup canceled (public)', path: () => '/payment-method-added?status=cancel', heading: () => 'Setup canceled', public: true },
]

// API calls that may fail on a healthy page, because the failure is the
// API's answer to the page's question. Each entry names the page state it
// belongs to.
const expectedFailures: { status: number; path: RegExp; signedOutOnly?: boolean }[] = [
  // A customer with no billing profile yet: 404 is "there is none yet", and
  // the page offers to add one.
  { status: 404, path: /^\/v1\/customers\/[^/]+\/billing-profile$/ },
  // Signed out: the session check answers 401, which is how the sign-in
  // pages learn that no one is signed in.
  { status: 401, path: /^\/v1\/whoami$/, signedOutOnly: true },
]

const viewports = [
  { name: 'desktop', size: { width: 1440, height: 900 } },
  { name: 'phone', size: { width: 390, height: 844 } },
]

function readSeed(): Seed {
  return JSON.parse(fs.readFileSync(seedFile, 'utf8')) as Seed
}

// Collects every problem the page reports while it loads.
function watchForProblems(page: Page, signedOut: boolean): string[] {
  const problems: string[] = []
  const expected = expectedFailures.filter(f => signedOut || !f.signedOutOnly)
  const isExpected = (url: string, status?: number) =>
    expected.some(f => f.path.test(new URL(url).pathname) && (status === undefined || f.status === status))
  page.on('console', msg => {
    if (msg.type() !== 'error') return
    // The browser logs every failed request to the console as well; those
    // are judged by the response check below.
    if (msg.text().startsWith('Failed to load resource') && isExpected(msg.location().url)) return
    problems.push(`console error: ${msg.text()}`)
  })
  page.on('pageerror', err => problems.push(`uncaught exception: ${err.message}`))
  page.on('response', res => {
    const url = new URL(res.url())
    if (!url.pathname.startsWith('/v1/') || res.status() < 400) return
    if (isExpected(res.url(), res.status())) return
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

// Pinned elements (sticky or fixed) whose background lets the page show
// through as it scrolls under them. A full-window overlay that dims the page
// behind a dialog is see-through on purpose and is left out. An element with
// no background of its own is judged by the first child that paints one.
function seeThroughPinned(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const clear = (c: string) => c === 'transparent' || /^rgba\(.*,\s*0\)$/.test(c)
    const alphaOf = (c: string) => {
      const m = c.match(/\/\s*([\d.]+)\s*\)$/) ?? c.match(/^rgba\(.*,\s*([\d.]+)\)$/)
      return m ? parseFloat(m[1]) : 1
    }
    const out: string[] = []
    for (const el of document.querySelectorAll<HTMLElement>('body *')) {
      const cs = getComputedStyle(el)
      if (cs.position !== 'sticky' && cs.position !== 'fixed') continue
      const r = el.getBoundingClientRect()
      if (r.width < 40 || r.height < 10 || cs.display === 'none' || cs.visibility === 'hidden') continue
      if (r.width >= innerWidth - 2 && r.height >= innerHeight - 2) continue
      let node: Element = el
      let bg = cs.backgroundColor
      while (clear(bg) && node.firstElementChild) {
        node = node.firstElementChild
        bg = getComputedStyle(node).backgroundColor
      }
      if (!clear(bg) && alphaOf(bg) < 0.98) out.push(`${el.tagName.toLowerCase()} "${el.innerText.slice(0, 40).replace(/\s+/g, ' ')}": ${bg}`)
    }
    return out
  })
}

// Text below the WCAG 2.2 AA contrast minimum: 4.5:1, or 3:1 for large text
// (24px, or 18.66px bold). Each element with its own text is measured against
// the colour behind it, with see-through layers and faded text blended in.
// Customer-facing pages measured 2.68:1 (a faded footer) and the dashboard
// 3.47:1 (a hard-coded red with no dark-mode colour) before this check.
function lowContrastText(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const canvas = document.createElement('canvas').getContext('2d')!
    const rgba = (c: string): number[] => {
      canvas.clearRect(0, 0, 1, 1)
      canvas.fillStyle = '#000'
      canvas.fillStyle = c
      canvas.fillRect(0, 0, 1, 1)
      const p = canvas.getImageData(0, 0, 1, 1).data
      return [p[0], p[1], p[2], p[3] / 255]
    }
    const lum = (rgb: number[]) => {
      const f = (x: number) => { x /= 255; return x <= 0.03928 ? x / 12.92 : ((x + 0.055) / 1.055) ** 2.4 }
      return 0.2126 * f(rgb[0]) + 0.7152 * f(rgb[1]) + 0.0722 * f(rgb[2])
    }
    const behind = (el: Element): number[] => {
      const layers: number[][] = []
      for (let n: Element | null = el; n; n = n.parentElement) {
        const c = rgba(getComputedStyle(n).backgroundColor)
        if (c[3] > 0) layers.push(c)
        if (c[3] >= 1) break
      }
      let base = [255, 255, 255]
      for (const c of layers.reverse()) base = base.map((v, i) => v * (1 - c[3]) + c[i] * c[3])
      return base
    }
    const out: string[] = []
    for (const el of document.querySelectorAll<HTMLElement>('body *')) {
      if (![...el.childNodes].some(n => n.nodeType === Node.TEXT_NODE && n.textContent?.trim())) continue
      const cs = getComputedStyle(el)
      if (cs.visibility === 'hidden' || cs.display === 'none' || el.getBoundingClientRect().width === 0) continue
      const bg = behind(el)
      const fg = rgba(cs.color)
      const text = fg.slice(0, 3).map((v, i) => v * fg[3] + bg[i] * (1 - fg[3]))
      const [hi, lo] = [lum(text), lum(bg)].sort((a, b) => b - a)
      const ratio = (hi + 0.05) / (lo + 0.05)
      const size = parseFloat(cs.fontSize)
      const large = size >= 24 || (size >= 18.66 && parseInt(cs.fontWeight) >= 700)
      if (ratio < (large ? 3 : 4.5)) out.push(`"${el.textContent!.trim().slice(0, 40)}": ${ratio.toFixed(2)}:1`)
    }
    return out
  })
}

for (const vp of viewports) {
  test.describe(`${vp.name} ${vp.size.width}px`, () => {
    test.use({ viewport: vp.size })

    for (const route of routes) {
      test(route.name, async ({ page }) => {
        const seed = readSeed()
        if (route.public) await page.context().clearCookies()
        const problems = watchForProblems(page, !!route.public)

        await page.goto(route.path(seed))
        if ('heading' in route) {
          await expect(page.getByRole('heading', { level: 1, name: route.heading(seed), exact: true })).toBeVisible()
        } else {
          await expect(page.getByText(route.text)).toBeVisible()
        }
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

        // Pinned bars stay solid, and text meets the contrast minimum, in both
        // themes: the test-mode band was a see-through tint in dark mode, and
        // every row scrolled visibly under it. The theme is the `dark` class
        // on <html> (useDarkMode, chosen once at load), so the check sets it
        // directly.
        // Colours change with a transition; measured mid-way, a dark page
        // reads as text on its own colour. Turn transitions off first.
        await page.addStyleTag({ content: '*, *::before, *::after { transition: none !important; animation: none !important }' })
        for (const theme of ['light', 'dark'] as const) {
          await page.evaluate(dark => document.documentElement.classList.toggle('dark', dark), theme === 'dark')
          expect(await seeThroughPinned(page), `see-through sticky or fixed bars (${theme})`).toEqual([])
          expect(await lowContrastText(page), `text below the WCAG AA contrast minimum (${theme})`).toEqual([])
        }
      })
    }
  })
}
