import { test, expect, type Locator, type Page, type Response } from '@playwright/test'
import { api, usd, uniq } from './fixtures/api'
import { createClock, createCustomer, createPlan, createSubscription, waitForClockReady, type Plan, type TestClock } from './fixtures/seed'

// Setting up what you sell and how you collect, through the screen: a meter,
// a plan and the plan's price; a ready-made recipe; one customer's negotiated
// price; and dunning policies. Each journey makes its own data, does the job on
// screen, then reads the API and checks that the page shows what the API
// holds. One test per kind of setup. The arithmetic behind the amounts is
// tested in Go.

// The API fields these journeys read, beyond the shared fixture's.
interface PlanPrice {
  id: string
  meter_id: string
  dimension_match: Record<string, string>
  mode: string
  unit_amount_cents: string
  tiers: { up_to: number | null; unit_amount_cents: string }[]
  package_size: number
  package_amount_cents: string
}
interface PriceWrite extends PlanPrice {
  effective: 'now' | 'next_period'
}
interface Meter {
  id: string
  event_name: string
  name: string
  unit: string
  aggregation: string
  has_usage?: boolean
}
interface PlanDetail extends Plan {
  currency: string
  billing_interval: string
  base_amount_cents: number
  base_bill_timing: string
  prices: PlanPrice[]
  used: boolean
}
interface PriceOverride {
  id: string
  price_id: string
  unit_amount_cents: string
  active: boolean
}
interface Preview {
  lines: { line_type: string; description: string; unit_amount_cents: number; amount_cents: number }[]
  totals: { currency: string; amount_cents: number }[]
}
interface RecipeInstance {
  id: string
  recipe_key: string
  overrides: Record<string, unknown>
  created_objects: { meter_ids?: string[]; price_ids?: string[]; plan_ids?: string[] }
}
interface RecipeListItem {
  key: string
  name: string
  overridable: { key: string; default?: unknown }[]
  instantiated?: RecipeInstance
}
interface RecipePreview {
  objects: {
    meters: { name: string; unit: string; aggregation: string }[]
    plans: {
      code: string; name: string; currency: string; billing_interval: string; base_amount_cents: number
      prices: { meter: string; dimension_match: Record<string, string> }[]
    }[]
  }
}
interface DunningPolicy {
  id: string
  name: string
  enabled: boolean
  is_default: boolean
  retry_days: number[]
  final_subscription_action: string
  final_invoice_action: string
  assigned_customers: number
}

function escapeRegExp(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}

// The next API call the page makes with this method to a path matching `path`.
function apiCall(page: Page, method: string, path: RegExp): Promise<Response> {
  return page.waitForResponse(r => r.request().method() === method && path.test(new URL(r.url()).pathname))
}

// Toasts render outside the page's main region.
function toasts(page: Page) {
  return page.getByRole('region', { name: /^Notifications/ })
}

function card(page: Page, title: string) {
  return page.getByRole('main').locator('[data-slot="card"]').filter({ has: page.getByText(title, { exact: true }) })
}

// A form field whose label is not tied to its input: the block holding the
// label text and the control.
function field(scope: Locator, label: string | RegExp) {
  return scope.getByText(label, { exact: true }).locator('..')
}

// Picks an option in a searchable picker that is already open.
async function pick(page: Page, search: string, option: string | RegExp) {
  await page.getByPlaceholder('Search...', { exact: true }).fill(search)
  await page.getByRole('option', { name: option }).click()
}

// Adds a price to the plan whose page is open, through its dialog, and
// returns the API's answer.
async function addPriceOnPage(page: Page, meterName: string, labels: [string, string][], fill: (dialog: Locator) => Promise<void>): Promise<PriceWrite> {
  const prices = page.getByRole('main').locator('[data-slot="card"]').filter({ has: page.getByText(/^Usage prices \(\d+\)$/) })
  await prices.getByRole('button', { name: 'Add price' }).click()
  const dialog = page.getByRole('dialog', { name: 'Add price' })
  await dialog.getByRole('button', { name: 'Select a meter…' }).click()
  await pick(page, meterName, new RegExp(`^${escapeRegExp(meterName)} \\(`))
  for (const [i, [k, v]] of labels.entries()) {
    await dialog.getByRole('button', { name: 'Add label' }).click()
    await dialog.getByLabel(`Label ${i + 1} key`).fill(k)
    await dialog.getByLabel(`Label ${i + 1} value`).fill(v)
  }
  await fill(dialog)
  const adding = apiCall(page, 'POST', /\/plans\/[^/]+\/prices$/)
  await dialog.getByRole('button', { name: 'Add price' }).click()
  const res = await adding
  expect(res.status()).toBe(201)
  await expect(dialog).toBeHidden()
  return (await res.json()) as PriceWrite
}

test('create a meter and a plan, then price the meter on the plan; the change applies at once while no one has subscribed', async ({ page }) => {
  const id = uniq()
  const meterName = `E2E UI calls ${id}`
  const meterKey = `e2e_ui_calls_${id}`
  const planName = `E2E UI plan ${id}`
  const planCode = `e2e-ui-plan-${id}`

  // A meter only counts. Saving opens the meter's page, which says no plan
  // prices it yet.
  await page.goto('/pricing?tab=meters')
  await page.getByRole('button', { name: 'Add Meter' }).click()
  const meterDialog = page.getByRole('dialog', { name: 'Create meter' })
  await field(meterDialog, 'Name').getByRole('textbox').fill(meterName)
  await field(meterDialog, 'Event name').getByRole('textbox').fill(meterKey)
  await field(meterDialog, 'Unit Label').getByRole('textbox').fill('call')
  await expect(meterDialog.getByText('Adds up the event values in the billing period. Fixed once the meter has a usage event.', { exact: true })).toBeVisible()
  const creatingMeter = apiCall(page, 'POST', /\/meters$/)
  await meterDialog.getByRole('button', { name: 'Create meter' }).click()
  const meterRes = await creatingMeter
  expect(meterRes.status()).toBe(201)
  const meter = (await meterRes.json()) as Meter
  expect([meter.event_name, meter.name, meter.unit, meter.aggregation, meter.has_usage]).toEqual([meterKey, meterName, 'call', 'sum', false])
  await expect(toasts(page).getByText('Meter created', { exact: true })).toBeVisible()
  await expect(page).toHaveURL(new RegExp(`/meters/${meter.id}$`))
  await expect(card(page, 'Plans that price this meter (0)')).toContainText("No plan prices this meter, so its usage isn't billed.")

  // A plan with a $29.00 base fee. With a base fee the form asks when it is
  // billed; saving opens the plan's page.
  await page.goto('/pricing')
  await page.getByRole('button', { name: 'Add Plan' }).click()
  const planDialog = page.getByRole('dialog', { name: 'Create plan' })
  await field(planDialog, 'Plan Name').getByRole('textbox').fill(planName)
  await planDialog.getByLabel('Code', { exact: true }).fill(planCode)
  await expect(planDialog.getByText('Base fee billed', { exact: true })).toBeHidden()
  await field(planDialog, /^Base Price/).getByRole('spinbutton').fill('29.00')
  await expect(planDialog.getByText('Base fee billed', { exact: true })).toBeVisible()
  const creatingPlan = apiCall(page, 'POST', /\/plans$/)
  await planDialog.getByRole('button', { name: 'Create plan' }).click()
  const planRes = await creatingPlan
  expect(planRes.status()).toBe(201)
  const created = (await planRes.json()) as PlanDetail
  await expect(toasts(page).getByText('Plan created', { exact: true })).toBeVisible()
  await expect(page).toHaveURL(new RegExp(`/plans/${created.id}$`))
  expect([created.name, created.code, created.currency, created.billing_interval, created.base_amount_cents, created.base_bill_timing, created.prices, created.used])
    .toEqual([planName, planCode, 'USD', 'monthly', 2900, 'in_arrears', [], false])

  // A price of $0.002 a call. No one has subscribed, so it applies at once,
  // and the card says so before the save.
  const prices = card(page, 'Usage prices (0)')
  await expect(prices.getByTestId('plan-prices-notice')).toHaveText('No one has subscribed to this plan yet, so price changes apply at once.')
  const written = await addPriceOnPage(page, meterName, [], async dialog => {
    await dialog.getByLabel(/^Price per unit/).fill('0.002')
  })
  // The dialog sends the rate in cents, exactly: $0.002 is 0.2 cents.
  expect([written.meter_id, written.dimension_match, written.mode, written.unit_amount_cents, written.effective]).toEqual([meter.id, {}, 'per_unit', '0.2', 'now'])
  await expect(toasts(page).getByText('Price saved.', { exact: true })).toBeVisible()
  const row = card(page, 'Usage prices (1)').getByTestId('plan-price-row')
  await expect(row).toHaveText(`${meterName}${meterKey}All events$0.002 / call`)

  // The plans list counts the price; the meter's page lists the plan.
  await page.goto('/pricing')
  const planRow = page.getByRole('row').filter({ hasText: planCode })
  await expect(planRow.getByRole('cell')).toHaveText([planName, planCode, 'monthly', 'active', usd(2900), '1'])
  await page.goto(`/meters/${meter.id}`)
  const pricedBy = card(page, 'Plans that price this meter (1)')
  await expect(pricedBy).toContainText(planName)
  await expect(pricedBy).toContainText('All events')
  await expect(pricedBy).toContainText('$0.002 / call')
})

// The upcoming-invoice card on the subscription page lists the lines the API
// previews, in order, with their amounts and the total.
async function expectUpcomingInvoiceMatchesAPI(page: Page, subscriptionId: string): Promise<Preview> {
  const { customer_id } = await api<{ customer_id: string }>('GET', `/subscriptions/${subscriptionId}`)
  const preview = await api<Preview>('POST', '/invoices/create_preview', { customer_id, subscription_id: subscriptionId })
  expect(preview.totals).toHaveLength(1)
  await page.goto(`/subscriptions/${subscriptionId}`)
  const upcoming = card(page, 'Upcoming invoice')
  await expect(upcoming.getByRole('row')).toHaveText([
    /^Description\s*Quantity\s*Amount$/,
    ...preview.lines.map(l => new RegExp(`^${escapeRegExp(l.description)}.*${escapeRegExp(usd(l.amount_cents))}$`)),
  ])
  await expect(upcoming).toContainText(usd(preview.totals[0].amount_cents))
  return preview
}

function usageRate(preview: Preview): number[] {
  return preview.lines.filter(l => l.line_type === 'usage').map(l => l.unit_amount_cents)
}

// Moves a clock a month and a day on, past the end of the period it is in.
async function advanceAPeriod(clock: TestClock) {
  const now = await api<TestClock>('GET', `/test-clocks/${clock.id}`)
  const to = new Date(now.frozen_time)
  to.setUTCMonth(to.getUTCMonth() + 1)
  to.setUTCDate(to.getUTCDate() + 1)
  await api('POST', `/test-clocks/${clock.id}/advance`, { frozen_time: to.toISOString().replace(/\.\d+Z$/, 'Z') })
  await waitForClockReady(clock.id)
}

// The price form writes each price shape in the API's words: a graduated
// price's last tier is open (up_to null), and a package price can be a
// fraction of a cent. Labels make each price take its own events.
// The server refuses a base fee of 0 on a plan billed in advance, and fixes
// the timing once a subscription has used the plan. So a used in_advance
// plan keeps a fee above 0, even once its subscriptions are canceled. The
// dialog said "0 for no base fee", showed the timing as "At end of period",
// and Save failed.
test('a used plan billed in advance keeps a base fee above 0, and the Edit plan dialog says so before saving', async ({ page }) => {
  const id = uniq()
  const plan = await api<PlanDetail>('POST', '/plans', {
    code: `e2e-adv-zero-${id}`, name: `E2E Advance zero ${id}`, currency: 'USD', billing_interval: 'monthly',
    base_amount_cents: 500, base_bill_timing: 'in_advance',
  })
  const customer = await createCustomer()
  const subscription = await createSubscription(customer.id, plan.id)
  await api('POST', `/subscriptions/${subscription.id}/cancel`)
  const stored = await api<PlanDetail & { live_subscription_count: number }>('GET', `/plans/${plan.id}`)
  expect([stored.used, stored.live_subscription_count, stored.base_bill_timing]).toEqual([true, 0, 'in_advance'])

  await page.goto(`/plans/${plan.id}`)
  await page.getByRole('button', { name: 'Edit', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Edit plan' })
  const timing = field(dialog, 'Base fee billed').getByRole('combobox')
  await expect(timing).toHaveText(/^At start of period/)
  await expect(timing).toBeDisabled()
  await expect(dialog.getByText('Monthly recurring charge, above 0.', { exact: true })).toBeVisible()
  await field(dialog, /^Base Price/).getByRole('spinbutton').fill('0')
  await expect(dialog.getByText('This plan bills its base fee at the start of each period, so the fee must be above 0. To charge no base fee, create a new plan.', { exact: true })).toBeVisible()
  await expect(timing).toHaveText(/^At start of period/)
  await expect(dialog.getByRole('button', { name: 'Save' })).toBeDisabled()
  await dialog.getByRole('button', { name: 'Cancel' }).click()

  // The server's refusal points at the same way out, not at a timing change
  // it would refuse too.
  const res = await page.request.fetch(`/v1/plans/${plan.id}`, { method: 'PATCH', data: { base_amount_cents: 0 } })
  expect(res.status()).toBe(422)
  expect(await res.text()).toContain('This plan bills its base fee in_advance, and a subscription has used it, so the base fee must stay above 0. To charge no base fee, create a new plan that bills in_arrears.')
})

test('the price form saves a graduated price with an open last tier, and a package price below a cent', async ({ page }) => {
  const id = uniq()
  const meter = await api<Meter>('POST', '/meters', { event_name: `e2e_shapes_${id}`, name: `E2E Shapes ${id}`, unit: 'unit', aggregation: 'sum' })
  const plan = await api<PlanDetail>('POST', '/plans', { code: `e2e-shapes-${id}`, name: `E2E Shapes plan ${id}`, currency: 'USD', billing_interval: 'monthly', base_amount_cents: 0 })
  await page.goto(`/plans/${plan.id}`)

  const tiered = await addPriceOnPage(page, meter.name, [['tier', 'a']], async dialog => {
    await dialog.getByRole('combobox', { name: 'Pricing model' }).click()
    await page.getByRole('option', { name: 'Graduated tiers' }).click()
    // The last tier has no upper limit, so its box takes no number.
    await expect(dialog.getByLabel('Tier 1 up to')).toBeEnabled()
    await expect(dialog.getByLabel('Tier 2 up to')).toBeDisabled()
    await expect(dialog.getByLabel('Tier 2 up to')).toHaveAttribute('placeholder', 'No limit')
    // A new price starts blank, so nothing is saved that nobody typed.
    await expect(dialog.getByLabel('Tier 1 up to')).toHaveValue('')
    await expect(dialog.getByLabel('Tier 1 price')).toHaveValue('')
    await dialog.getByRole('button', { name: 'Add price' }).click()
    await expect(dialog.getByText('Tier 1: enter how many units the tier goes up to', { exact: false })).toBeVisible()
    await dialog.getByLabel('Tier 1 up to').fill('1000')
    await dialog.getByLabel('Tier 1 price').fill('0.10')
    await dialog.getByLabel('Tier 2 price').fill('0.05')
  })
  expect([tiered.dimension_match, tiered.mode, tiered.tiers]).toEqual([{ tier: 'a' }, 'graduated', [{ up_to: 1000, unit_amount_cents: '10' }, { up_to: null, unit_amount_cents: '5' }]])
  await expect(card(page, 'Usage prices (1)').getByTestId('plan-price-row').filter({ hasText: 'tier=a' })).toHaveText(
    `${meter.name}${meter.event_name}tier=atiered / unit: up to 1,000 @ $0.10, beyond @ $0.05`)

  const pkg = await addPriceOnPage(page, meter.name, [['tier', 'b']], async dialog => {
    await dialog.getByRole('combobox', { name: 'Pricing model' }).click()
    await page.getByRole('option', { name: 'Per package' }).click()
    await expect(dialog.getByText('A part block is charged as a whole one.', { exact: false })).toBeVisible()
    await expect(dialog.getByLabel('Units per package')).toHaveValue('')
    await expect(dialog.getByLabel(/^Price per package/)).toHaveValue('')
    await dialog.getByLabel('Units per package').fill('1000')
    await dialog.getByLabel(/^Price per package/).fill('0.002')
  })
  // $0.002 a package is 0.2 cents, exactly.
  expect([pkg.dimension_match, pkg.mode, pkg.package_size, pkg.package_amount_cents]).toEqual([{ tier: 'b' }, 'package', 1000, '0.2'])
  await expect(card(page, 'Usage prices (2)').getByTestId('plan-price-row').filter({ hasText: 'tier=b' })).toHaveText(
    `${meter.name}${meter.event_name}tier=b$0.002 per 1,000 unit`)
})

// A meter's aggregation is fixed once the meter has usage events (ADR-130
// §2). The meter page says so before any edit, and the edit dialog then
// offers the aggregation read-only; before the first event it can change.
test('the meter page shows the aggregation lock before an edit: free with no usage, fixed once usage arrives', async ({ page }) => {
  const id = uniq()
  const meter = await api<Meter>('POST', '/meters', {
    event_name: `e2e_lock_${id}`, name: `E2E Lock ${id}`, unit: 'call', aggregation: 'sum',
  })
  expect(meter.has_usage).toBe(false)

  await page.goto(`/meters/${meter.id}`)
  const lock = page.getByTestId('meter-aggregation-lock')
  await expect(lock).toHaveText('Adds up the event values in the billing period. The aggregation can change until this meter has a usage event. Held usage does not fix it.')
  await page.getByRole('button', { name: 'Edit', exact: true }).click()
  let dialog = page.getByRole('dialog', { name: 'Edit meter' })
  await expect(dialog.getByRole('combobox', { name: 'Aggregation' })).toBeEnabled()
  await dialog.getByRole('button', { name: 'Cancel' }).click()

  const customer = await createCustomer()
  await api('POST', '/usage-events', { external_customer_id: customer.external_id, event_name: meter.event_name, quantity: '3', idempotency_key: `e2e_${uniq()}` })
  expect((await api<Meter>('GET', `/meters/${meter.id}`)).has_usage).toBe(true)

  await page.reload()
  await expect(lock).toHaveText('Adds up the event values in the billing period. This meter has usage events, so its aggregation is fixed. Make a new meter to count differently.')
  await page.getByRole('button', { name: 'Edit', exact: true }).click()
  dialog = page.getByRole('dialog', { name: 'Edit meter' })
  await expect(dialog.getByRole('combobox', { name: 'Aggregation' })).toBeDisabled()
  await expect(dialog.getByText('This meter has usage events, so its aggregation is fixed. Make a new meter to count differently.', { exact: true })).toBeVisible()
})

test('a price override for one customer: the upcoming invoice bills the negotiated rate from the next period, and the list rate once it ends', async ({ page }) => {
  const id = uniq()
  const meter = await api<Meter>('POST', '/meters', {
    event_name: `e2e_deal_${id}`, name: `E2E Deal calls ${id}`, unit: 'call', aggregation: 'sum',
  })
  const plan = await api<PlanDetail>('POST', '/plans', {
    code: `e2e-deal-${id}`, name: `E2E Deal plan ${id}`, currency: 'USD', billing_interval: 'monthly', base_amount_cents: 0,
    prices: [{ meter_id: meter.id, mode: 'per_unit', unit_amount_cents: '2' }],
  })
  // A plan the customer is not on: its price is listed after the customer's.
  await api<PlanDetail>('POST', '/plans', {
    code: `e2e-deal-other-${id}`, name: `E2E Deal other plan ${id}`, currency: 'USD', billing_interval: 'monthly', base_amount_cents: 0,
    prices: [{ meter_id: meter.id, mode: 'per_unit', unit_amount_cents: '5' }],
  })
  const clock = await createClock()
  const customer = await createCustomer(clock.id)
  const subscription = await createSubscription(customer.id, plan.id)
  const ingest = () => api('POST', '/usage-events', { external_customer_id: customer.external_id, event_name: meter.event_name, quantity: '500', idempotency_key: `e2e_${uniq()}` })
  await ingest()

  // The plan has a subscriber now, so its page says price changes wait for
  // each subscriber's next period.
  await page.goto(`/plans/${plan.id}`)
  await expect(card(page, 'Usage prices (1)').getByTestId('plan-prices-notice')).toHaveText(
    "Price changes apply from each subscriber's next billing period. A subscriber who joins this plan after a change, by subscribing, moving to it or adding it, pays the new prices from then, unless the same subscription was on this plan earlier in that billing period. On a test clock, advance the clock first.")

  // Control: the list rate, 2 cents a call.
  expect(usageRate(await expectUpcomingInvoiceMatchesAPI(page, subscription.id))).toEqual([2])

  // Add a negotiated rate of $0.01.
  const reason = `E2E annual deal ${id}`
  await page.goto(`/customers/${customer.id}`)
  const overrides = card(page, 'Price overrides')
  await expect(overrides).toContainText('No negotiated prices — this customer bills at list price.')
  await overrides.getByRole('button', { name: 'Add override' }).click()
  const add = page.getByRole('dialog', { name: 'Add price override' })
  await add.getByRole('button', { name: 'Select a plan price...' }).click()
  const priceName = `${plan.name} · ${meter.name}`
  // The prices of the customer's own plans come first, and the form says so.
  await expect(page.getByRole('option').first()).toHaveText(new RegExp(`^${escapeRegExp(`${priceName} — list $0.02 / call`)}`))
  await expect(add.getByText("Prices of this customer's plans are listed first.", { exact: true })).toBeVisible()
  await pick(page, plan.name, new RegExp(`^${escapeRegExp(`${priceName} — list $0.02 / call`)}`))
  await add.getByLabel(/^Price per unit/).fill('0.01')
  await expect(add.getByText('List price: $0.02 / call.', { exact: false })).toBeVisible()
  await expect(add).toContainText('If the customer subscribes to the plan or moves to it after you save, it applies from then, unless the same subscription was on the plan earlier in that billing period.')
  await add.getByLabel('Reason (optional)').fill(reason)
  const saving = apiCall(page, 'POST', /\/price-overrides$/)
  await add.getByRole('button', { name: 'Save override' }).click()
  const savedRes = await saving
  expect(savedRes.status()).toBe(201)
  const saved = (await savedRes.json()) as PriceOverride
  expect([saved.price_id, saved.unit_amount_cents, saved.active]).toEqual([plan.prices[0].id, '1', true])
  await expect(add).toBeHidden()
  const active = async () => (await api<{ data: PriceOverride[] }>('GET', `/price-overrides?customer_id=${customer.id}`)).data.filter(o => o.active)
  expect((await active()).map(o => o.id)).toEqual([saved.id])
  await expect(overrides).toContainText(priceName)
  await expect(overrides).toContainText('$0.01 / call · list price $0.02 / call')
  await expect(overrides).toContainText(`${reason} · since`)

  // The period in progress keeps the list rate; the next one bills the deal.
  expect(usageRate(await expectUpcomingInvoiceMatchesAPI(page, subscription.id))).toEqual([2])
  await advanceAPeriod(clock)
  await ingest()
  expect(usageRate(await expectUpcomingInvoiceMatchesAPI(page, subscription.id))).toEqual([1])

  // End it: the card is empty again, and the list rate is back from the
  // next period.
  await page.goto(`/customers/${customer.id}`)
  await overrides.getByRole('button', { name: `End override on ${priceName}` }).click()
  const end = page.getByRole('dialog', { name: 'End price override' })
  await expect(end).toContainText(`${priceName} returns to list price for this customer.`)
  await expect(end).toContainText('If the customer subscribes to the plan or moves to it after you end the override, list price applies from then, unless the same subscription was on the plan earlier in that billing period.')
  const ending = apiCall(page, 'DELETE', new RegExp(`/price-overrides/${saved.id}$`))
  await end.getByRole('button', { name: 'End override' }).click()
  expect((await ending).ok()).toBe(true)
  await expect(end).toBeHidden()
  expect(await active()).toEqual([])
  await expect(overrides).toContainText('No negotiated prices — this customer bills at list price.')

  expect(usageRate(await expectUpcomingInvoiceMatchesAPI(page, subscription.id))).toEqual([1])
  await advanceAPeriod(clock)
  await ingest()
  expect(usageRate(await expectUpcomingInvoiceMatchesAPI(page, subscription.id))).toEqual([2])
})

test('install a recipe: the preview lists what it will create, the install creates it once, and installing again changes nothing', async ({ page }) => {
  const KEY = 'replicate_style'
  const listed = (await api<{ data: RecipeListItem[] }>('GET', '/recipes')).data.find(r => r.key === KEY)
  expect(listed, `${KEY} is a bundled recipe`).toBeDefined()
  const recipe = listed!
  const dialog = page.getByRole('dialog', { name: recipe.name })
  let instance: RecipeInstance

  // A recipe installs once per workspace. On a re-run against the same
  // database it is already installed, and only the checks after this
  // block apply.
  if (!recipe.instantiated) {
    const id = uniq()
    const planName = `E2E GPU ${id}`
    const planCode = `e2e_gpu_${id}`
    await page.goto('/recipes')
    await card(page, KEY).getByRole('button', { name: 'Configure' }).click()
    await dialog.getByLabel('Plan name', { exact: true }).fill(planName)
    await dialog.getByLabel('Plan ID', { exact: true }).fill(planCode)

    // The preview lists what the API says the install will create, with
    // the plan name and code just typed.
    const previewing = apiCall(page, 'POST', new RegExp(`/recipes/${KEY}/preview$`))
    await dialog.getByRole('button', { name: 'Preview', exact: true }).click()
    const previewRes = await previewing
    expect(previewRes.ok()).toBe(true)
    const { objects } = (await previewRes.json()) as RecipePreview
    expect(objects.plans.map(p => [p.name, p.code])).toEqual([[planName, planCode]])
    const section = (label: string) => field(dialog, label).getByRole('listitem')
    await expect(section('What gets counted')).toHaveText(objects.meters.map(m => `· ${m.name} (${m.aggregation} of ${m.unit})`))
    const labels = (m: Record<string, string>) => Object.entries(m ?? {}).map(([k, v]) => `${k}=${v}`).join(', ')
    await expect(section('Prices')).toHaveText(objects.plans.flatMap(plan => plan.prices).map(p =>
      new RegExp(`^· ${escapeRegExp(p.meter + (labels(p.dimension_match) ? ` (${labels(p.dimension_match)})` : ''))} — `)))
    await expect(section('Plan')).toHaveText(objects.plans.map(p =>
      `· ${p.name} — ${p.billing_interval}, ${p.currency}${p.base_amount_cents > 0 ? `, ${usd(p.base_amount_cents)} base` : ''}`))
    // A recipe creates only the catalog: no retry policy, no webhook
    // endpoint (ADR-130 §15).
    expect(Object.keys(objects).sort()).toEqual(['meters', 'plans'])
    await expect(dialog.getByText('Payment retries')).toHaveCount(0)

    // Install: the page lands on the new plan.
    const installing = apiCall(page, 'POST', new RegExp(`/recipes/${KEY}/instantiate$`))
    await dialog.getByRole('button', { name: 'Install recipe' }).click()
    const installRes = await installing
    expect(installRes.status()).toBe(201)
    instance = (await installRes.json()) as RecipeInstance
    await expect(toasts(page).getByText(`Installed ${recipe.name}`, { exact: true })).toBeVisible()
    const planId = instance.created_objects.plan_ids?.[0]
    await expect(page).toHaveURL(new RegExp(`/plans/${planId}$`))
    const plan = await api<PlanDetail>('GET', `/plans/${planId}`)
    expect([plan.name, plan.code, [...new Set(plan.prices.map(p => p.meter_id))], plan.prices.map(p => p.id)])
      .toEqual([planName, planCode, instance.created_objects.meter_ids, instance.created_objects.price_ids])
    await expect(page.getByRole('heading', { level: 1, name: planName, exact: true })).toBeVisible()
    const prices = card(page, `Usage prices (${plan.prices.length})`)
    await expect(prices).toContainText(objects.meters[0].name)
    // The install made only catalog objects.
    expect(Object.keys(instance.created_objects).sort()).toEqual(['meter_ids', 'plan_ids', 'price_ids'])
  } else {
    instance = recipe.instantiated
  }
  // An install records only the settings it was given; the rest kept
  // their defaults.
  const planName = String(instance.overrides.plan_name ?? recipe.overridable.find(o => o.key === 'plan_name')?.default)

  // The recipe reads as installed, with the settings it was installed
  // with, and cannot be installed again from the page.
  await page.goto('/recipes')
  const installed = card(page, KEY)
  await expect(installed.getByText('Installed', { exact: true })).toBeVisible()
  // The settings are fixed once installed, so the card no longer offers them.
  const n = recipe.overridable.length
  await expect(installed.getByText(`${n} setting${n !== 1 ? 's' : ''} set at install`, { exact: true })).toBeVisible()
  await installed.getByRole('button', { name: 'View' }).click()
  await expect(dialog.getByText('Installed with these settings', { exact: true })).toBeVisible()
  await expect(dialog.getByText("Set at install and fixed here. On the plan's page you can rename the plan and edit its prices. Its currency and plan ID cannot change.", { exact: true })).toBeVisible()
  await expect(dialog.getByLabel('Plan name', { exact: true })).toHaveValue(planName)
  await expect(dialog.getByLabel('Plan name', { exact: true })).toBeDisabled()
  await expect(dialog.getByRole('button', { name: 'Already installed' })).toBeDisabled()
  await dialog.getByRole('button', { name: 'Cancel' }).click()

  // Installing again through the API returns the same install and makes
  // nothing new (ADR-085).
  const again = await api<RecipeInstance>('POST', `/recipes/${KEY}/instantiate`, {})
  expect(again.id).toBe(instance.id)
  const instances = (await api<{ data: RecipeInstance[] }>('GET', '/recipes/instances')).data
  expect(instances.filter(i => i.recipe_key === KEY).map(i => i.id)).toEqual([instance.id])
  const plans = (await api<{ data: PlanDetail[] }>('GET', '/plans')).data
  expect(plans.filter(p => p.name === planName).map(p => p.id)).toEqual(instance.created_objects.plan_ids)
  const meters = (await api<{ data: Meter[] }>('GET', '/meters')).data
  expect(meters.filter(m => m.event_name === 'gpu_seconds').map(m => m.id)).toEqual(instance.created_objects.meter_ids)
})

test('dunning policies: create one turned off, turn it on and edit it, give it to a customer (it cannot be deleted then), and make it the default', async ({ page }) => {
  const id = uniq()
  const name = `E2E retries ${id}`
  const customer = await createCustomer()
  const policies = async () => (await api<{ data: DunningPolicy[] }>('GET', '/dunning/policies')).data
  const policy = async (policyId: string) => (await policies()).find(p => p.id === policyId)
  // A policy's row in the policies table, and its cells by column:
  // Name, Retries, When retries run out, Customers, Status, actions.
  const policyRow = (policyName: string) => page.getByRole('main').getByRole('row')
    .filter({ has: page.getByText(policyName, { exact: true }) })
  const cell = (policyName: string, column: number) => policyRow(policyName).getByRole('cell').nth(column)
  // The row's actions are in its menu.
  const menu = async (policyName: string) => {
    await page.getByRole('button', { name: `Actions for policy ${policyName}`, exact: true }).click()
    return page.getByRole('menu')
  }
  let restoreDefault: string | undefined

  try {
    // Created turned off, so it cannot take the default on its own
    // (ADR-123); it leaves the subscription and invoice alone when retries
    // run out.
    await page.goto('/dunning-policies')
    await page.getByRole('main').getByRole('button', { name: 'New policy' }).first().click()
    const create = page.getByRole('dialog', { name: 'New dunning policy' })
    await field(create, 'Name').getByRole('textbox').fill(name)
    await create.getByRole('switch').click()
    await expect(create.getByRole('switch')).not.toBeChecked()
    // One list of waits in days: the first after the failed payment, each
    // next one after the retry before it.
    await field(create, 'Days before each retry').getByRole('textbox').fill('2, 3, 5')
    await expect(create.getByText('3 retries, on day 2, 5 and 10 after the failed payment or missing card.', { exact: true })).toBeVisible()
    // The default, pause, keeps new invoices as drafts; runs already open on
    // other unpaid invoices keep retrying, so it must not say charging stops.
    await expect(create.getByText(/^Invoices created while collection is paused stay drafts, so nothing charges them\. Retries already running on other unpaid invoices continue\./)).toBeVisible()
    await field(create, 'Do this to the subscription').getByRole('combobox').click()
    await page.getByRole('option', { name: 'Leave it active' }).click()
    await expect(create.getByText(/^The subscription keeps billing as normal\. The unpaid invoice stays open and due\./)).toBeVisible()
    const creating = apiCall(page, 'POST', /\/dunning\/policies$/)
    await create.getByRole('button', { name: 'Save' }).click()
    const createdRes = await creating
    expect(createdRes.status()).toBe(201)
    const created = (await createdRes.json()) as DunningPolicy
    expect(created).toMatchObject({
      name, enabled: false, is_default: false, retry_days: [2, 3, 5],
      final_subscription_action: 'none', final_invoice_action: 'none',
    })
    await expect(toasts(page).getByText('Policy created', { exact: true })).toBeVisible()
    await expect(cell(name, 1)).toHaveText('3 retries, on day 2, 5 and 10')
    await expect(cell(name, 2)).toHaveText('No action')
    await expect(cell(name, 3)).toHaveText('0')
    await expect(cell(name, 4)).toHaveText('Off')

    // Turn it on with four retries: a fourth wait is a fourth retry. A
    // wait that is not whole days is refused before it is sent.
    await (await menu(name)).getByRole('menuitem', { name: 'Edit', exact: true }).click()
    const edit = page.getByRole('dialog', { name: 'Edit policy' })
    await edit.getByRole('switch').click()
    await expect(edit.getByRole('switch')).toBeChecked()
    const days = field(edit, 'Days before each retry').getByRole('textbox')
    await days.fill('2, 3, 36h')
    await expect(edit.getByText('Each wait is a whole number of days from 1 to 30.', { exact: true })).toBeVisible()
    await expect(edit.getByRole('button', { name: 'Save' })).toBeDisabled()
    await days.fill('2, 3, 5, 7')
    await expect(edit.getByText('4 retries, on day 2, 5, 10 and 17 after the failed payment or missing card.', { exact: true })).toBeVisible()
    const saving = apiCall(page, 'PATCH', new RegExp(`/dunning/policies/${created.id}$`))
    await edit.getByRole('button', { name: 'Save' }).click()
    expect((await saving).ok()).toBe(true)
    await expect(toasts(page).getByText('Policy saved', { exact: true })).toBeVisible()
    expect(await policy(created.id)).toMatchObject({
      enabled: true, is_default: false, retry_days: [2, 3, 5, 7],
    })
    await expect(cell(name, 4)).toHaveText('On')
    await expect(cell(name, 1)).toHaveText('4 retries, on day 2, 5, 10 and 17')

    // Give it to one customer: their page names it instead of the default.
    await page.goto(`/customers/${customer.id}`)
    const dunning = card(page, 'Dunning policy')
    await expect(dunning.getByText('account default', { exact: true })).toBeVisible()
    await dunning.getByRole('button', { name: 'Change' }).click()
    const assign = page.getByRole('dialog', { name: 'Dunning policy' })
    await assign.getByRole('button', { name: /^Account default/ }).click()
    await pick(page, name, name)
    const assigning = apiCall(page, 'PATCH', new RegExp(`/customers/${customer.id}$`))
    await assign.getByRole('button', { name: 'Save' }).click()
    expect((await assigning).ok()).toBe(true)
    await expect(toasts(page).getByText('Dunning policy assignment saved', { exact: true })).toBeVisible()
    expect((await api<{ dunning_policy_id?: string }>('GET', `/customers/${customer.id}`)).dunning_policy_id).toBe(created.id)
    await expect(dunning).toContainText(name)
    await expect(dunning.getByText('account default', { exact: true })).toHaveCount(0)
    await expect(field(dunning, 'Schedule')).toContainText('4 retries, on day 2, 5, 10 and 17')

    // While a customer has it, the policy cannot be deleted: the page says
    // why and asks nothing.
    await page.goto('/dunning-policies')
    expect((await policy(created.id))?.assigned_customers).toBe(1)
    await expect(cell(name, 3)).toHaveText('1')
    await (await menu(name)).getByRole('menuitem', { name: 'Delete', exact: true }).click()
    await expect(toasts(page).getByText('1 customer(s) assigned — reassign them first', { exact: true })).toBeVisible()
    await expect(page.getByRole('alertdialog')).toHaveCount(0)
    expect(await policy(created.id)).toBeDefined()

    // Make it the default: the badge moves from the old default to it.
    const previous = (await policies()).find(p => p.is_default)
    restoreDefault = previous?.id
    const settingDefault = apiCall(page, 'POST', new RegExp(`/dunning/policies/${created.id}/set-default$`))
    await (await menu(name)).getByRole('menuitem', { name: 'Make default', exact: true }).click()
    expect((await settingDefault).ok()).toBe(true)
    await expect(toasts(page).getByText('Default policy updated', { exact: true })).toBeVisible()
    expect((await policies()).filter(p => p.is_default).map(p => p.id)).toEqual([created.id])
    await expect(policyRow(name).getByText('Default', { exact: true })).toBeVisible()
    // The default can be edited, not made default again or deleted.
    const own = await menu(name)
    await expect(own.getByRole('menuitem', { name: 'Edit', exact: true })).toBeVisible()
    await expect(own.getByRole('menuitem', { name: 'Make default', exact: true })).toHaveCount(0)
    await expect(own.getByRole('menuitem', { name: 'Delete', exact: true })).toHaveCount(0)
    await page.keyboard.press('Escape')
    if (previous?.name) {
      await expect(policyRow(previous.name).getByText('Default', { exact: true })).toHaveCount(0)
      await expect((await menu(previous.name)).getByRole('menuitem', { name: 'Make default', exact: true })).toBeVisible()
      await page.keyboard.press('Escape')
    }
  } finally {
    // Every test shares this workspace's default: put the old one back.
    if (restoreDefault) await api('POST', `/dunning/policies/${restoreDefault}/set-default`)
  }
})

// A plan priced per model and token type reads as a rate card: one row per
// model, a column per token type, prices per 1M tokens. A label only some
// prices carry is a tag on the row, search narrows the rows, and a price opens
// to change or remove it. An AI plan's 125 prices were 125 stacked blocks.
test('a plan priced per model and token type reads as a rate card, with search', async ({ page }) => {
  const id = uniq()
  const plan = await createPlan()
  const meter = await api<{ id: string; name: string }>('POST', '/meters', { event_name: `e2e_tokens_${id}`, name: `E2E Tokens ${id}`, unit: 'tokens', aggregation: 'sum' })
  const rates: [Record<string, string>, string][] = [
    [{ model: 'alpha', token_type: 'input' }, '0.0003'], [{ model: 'alpha', token_type: 'output' }, '0.0015'],
    [{ model: 'beta', token_type: 'input' }, '0.0001'], [{ model: 'beta', token_type: 'output' }, '0.0005'],
    [{ model: 'gamma', context_tier: 'long', token_type: 'input' }, '0.0006'], [{ model: 'gamma', context_tier: 'long', token_type: 'output' }, '0.003'],
  ]
  for (const [match, cents] of rates) {
    await api('POST', `/plans/${plan.id}/prices`, { meter_id: meter.id, dimension_match: match, mode: 'per_unit', unit_amount_cents: cents })
  }

  await page.goto(`/plans/${plan.id}`)
  const prices = card(page, 'Usage prices (6)')
  const rateCard = prices.getByTestId('plan-rate-card')
  await expect(rateCard.getByRole('columnheader')).toHaveText(['model', 'input', 'output'])
  // 0.0003 cents a token is $3.00 per 1M tokens.
  await expect(rateCard.getByRole('row')).toHaveText([
    /^model\s*input\s*output$/,
    /^alpha\s*\$3\.00\s*\$15\.00$/,
    /^beta\s*\$1\.00\s*\$5\.00$/,
    /^gamma\s*context_tier=long\s*\$6\.00\s*\$30\.00$/,
  ])
  await expect(prices.getByTestId('plan-price-row')).toHaveCount(0)

  // Six prices fit at a glance, so there is no search box; the cell opens the
  // price, which can be changed or removed from there.
  await expect(prices.getByLabel('Search prices')).toHaveCount(0)
  await rateCard.getByRole('button', { name: 'Change price: model=beta, token_type=output' }).click()
  const dialog = page.getByRole('dialog', { name: 'Change price' })
  await expect(dialog.getByRole('button', { name: 'Remove price' })).toBeVisible()
  await dialog.getByRole('button', { name: 'Cancel' }).click()

  // More prices than a glance takes in: search narrows the rows.
  for (const model of ['delta', 'epsilon']) {
    for (const t of ['input', 'output']) {
      await api('POST', `/plans/${plan.id}/prices`, { meter_id: meter.id, dimension_match: { model, token_type: t }, mode: 'per_unit', unit_amount_cents: '0.0002' })
    }
  }
  await page.reload()
  const more = card(page, 'Usage prices (10)')
  await more.getByLabel('Search prices').fill('gam')
  await expect(more.getByTestId('plan-rate-card').getByRole('row')).toHaveCount(2)
  await more.getByLabel('Search prices').fill('zeta')
  await expect(more.getByText('No price matches "zeta".')).toBeVisible()
})
