import { test, expect, type Locator, type Page, type Response } from '@playwright/test'
import { api, usd, uniq } from './fixtures/api'
import { createClock, createCustomer, createSubscription, waitForClockReady, type Plan, type TestClock } from './fixtures/seed'

// Setting up what you sell and how you collect, through the screen: a price,
// a meter and a plan; a ready-made recipe; one customer's negotiated price;
// and dunning policies. Each journey makes its own data, does the job on
// screen, then reads the API and checks that the page shows what the API
// holds. One test per kind of setup. The arithmetic behind the amounts is
// tested in Go.

// The API fields these journeys read, beyond the shared fixture's.
interface RatingRule {
  id: string
  rule_key: string
  name: string
  version: number
  mode: string
  currency: string
  flat_amount_cents: string
}
interface Meter {
  id: string
  key: string
  name: string
  unit: string
  aggregation: string
  rating_rule_version_id?: string
}
interface PlanDetail extends Plan {
  currency: string
  billing_interval: string
  base_amount_cents: number
  base_bill_timing: string
  meter_ids: string[]
}
interface PriceOverride {
  id: string
  rule_key: string
  flat_amount_cents: string
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
  created_objects: { meter_ids?: string[]; pricing_rule_ids?: string[]; plan_ids?: string[] }
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
    rating_rules: { key: string; name: string }[]
    plans: { code: string; name: string; currency: string; billing_interval: string; base_amount_cents: number }[]
    dunning_policies: { name: string; max_retries: number }[]
    webhook_endpoints: { url_placeholder: string }[]
  }
}
interface DunningPolicy {
  id: string
  name: string
  enabled: boolean
  is_default: boolean
  max_retry_attempts: number
  grace_period_days: number
  retry_schedule: string[]
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

test('create a price, a meter that bills at it, and a plan with both; a meter with no price is refused on the plan', async ({ page }) => {
  const id = uniq()
  const priceName = `E2E UI price ${id}`
  const priceKey = `e2e_ui_price_${id}`
  const meterName = `E2E UI calls ${id}`
  const meterKey = `e2e_ui_calls_${id}`
  const planName = `E2E UI plan ${id}`
  const planCode = `e2e-ui-plan-${id}`
  // Usage on a meter with no price would bill nothing, so a plan refuses it
  // (ADR-096).
  const unpriced = await api<Meter>('POST', '/meters', { key: `e2e_unpriced_${id}`, name: `E2E Unpriced ${id}`, unit: 'call', aggregation: 'sum' })

  // A flat price of $0.002 per unit.
  await page.goto('/pricing?tab=rules')
  await page.getByRole('button', { name: 'Add Price' }).click()
  const priceDialog = page.getByRole('dialog', { name: 'Create price' })
  await priceDialog.getByLabel('Name', { exact: true }).fill(priceName)
  await priceDialog.getByLabel('Key', { exact: true }).fill(priceKey)
  await priceDialog.getByLabel(/^Price per Unit/).fill('0.002')
  const creatingPrice = apiCall(page, 'POST', /\/rating-rules$/)
  await priceDialog.getByRole('button', { name: 'Create price' }).click()
  const priceRes = await creatingPrice
  expect(priceRes.status()).toBe(201)
  const rule = (await priceRes.json()) as RatingRule
  // The dialog sends the rate in cents, exactly: $0.002 is 0.2 cents.
  expect([rule.rule_key, rule.name, rule.mode, rule.flat_amount_cents, rule.version, rule.currency]).toEqual([priceKey, priceName, 'flat', '0.2', 1, 'USD'])
  await expect(toasts(page).getByText('Price created', { exact: true })).toBeVisible()
  await page.getByLabel('Search prices').fill(priceKey)
  const priceRow = page.getByRole('row').filter({ hasText: priceKey })
  await expect(priceRow.getByRole('cell')).toHaveText([priceName, priceKey, 'flat', 'v1', 'Not linked', /^\$0\.002 \/ unit/])

  // A meter that bills at that price. Saving opens the meter's page.
  await page.goto('/pricing?tab=meters')
  await page.getByRole('button', { name: 'Add Meter' }).click()
  const meterDialog = page.getByRole('dialog', { name: 'Create meter' })
  await field(meterDialog, 'Name').getByRole('textbox').fill(meterName)
  await field(meterDialog, 'Key').getByRole('textbox').fill(meterKey)
  await field(meterDialog, 'Unit Label').getByRole('textbox').fill('call')
  await expect(meterDialog.getByText('Add up all values in the billing period', { exact: true })).toBeVisible()
  await meterDialog.getByRole('button', { name: 'None (assign later)' }).click()
  await pick(page, priceName, `${priceName} — $0.002 / call`)
  const creatingMeter = apiCall(page, 'POST', /\/meters$/)
  await meterDialog.getByRole('button', { name: 'Create meter' }).click()
  const meterRes = await creatingMeter
  expect(meterRes.status()).toBe(201)
  const meter = (await meterRes.json()) as Meter
  expect([meter.key, meter.name, meter.unit, meter.aggregation, meter.rating_rule_version_id]).toEqual([meterKey, meterName, 'call', 'sum', rule.id])
  await expect(toasts(page).getByText('Meter created', { exact: true })).toBeVisible()
  await expect(page).toHaveURL(new RegExp(`/meters/${meter.id}$`))
  await expect(page.getByRole('heading', { level: 1, name: meterName, exact: true })).toBeVisible()
  const defaultPrice = card(page, 'Default price')
  await expect(defaultPrice).toContainText(priceName)
  await expect(defaultPrice).toContainText('$0.002')
  await expect(defaultPrice).toContainText('per call')

  // A plan with a $29.00 base fee. With the unpriced meter ticked too, the
  // API refuses the plan and the toast gives its reason; the dialog stays
  // open, and no plan is made.
  await page.goto('/pricing')
  await page.getByRole('button', { name: 'Add Plan' }).click()
  const planDialog = page.getByRole('dialog', { name: 'Create plan' })
  await field(planDialog, 'Plan Name').getByRole('textbox').fill(planName)
  await planDialog.getByLabel('Code', { exact: true }).fill(planCode)
  await field(planDialog, /^Base Price/).getByRole('spinbutton').fill('29.00')
  await planDialog.getByRole('checkbox', { name: new RegExp(meterKey) }).check()
  await planDialog.getByRole('checkbox', { name: new RegExp(unpriced.key) }).check()
  const refusing = apiCall(page, 'POST', /\/plans$/)
  await planDialog.getByRole('button', { name: 'Create plan' }).click()
  const refusal = await refusing
  expect(refusal.status()).toBe(422)
  const { error } = (await refusal.json()) as { error: { message: string; param: string } }
  expect(error.param).toBe('meter_ids')
  expect(error.message).toContain(`"${unpriced.name}" has no price`)
  await expect(toasts(page).getByText(error.message, { exact: true })).toBeVisible()
  await expect(planDialog).toBeVisible()
  expect((await api<{ data: Plan[] }>('GET', '/plans')).data.filter(p => p.code === planCode)).toEqual([])

  // Without it, the plan is made with the priced meter.
  await planDialog.getByRole('checkbox', { name: new RegExp(unpriced.key) }).uncheck()
  const creatingPlan = apiCall(page, 'POST', /\/plans$/)
  await planDialog.getByRole('button', { name: 'Create plan' }).click()
  const planRes = await creatingPlan
  expect(planRes.status()).toBe(201)
  await expect(toasts(page).getByText('Plan created', { exact: true })).toBeVisible()
  await expect(planDialog).toBeHidden()
  const plan = await api<PlanDetail>('GET', `/plans/${((await planRes.json()) as PlanDetail).id}`)
  expect([plan.name, plan.code, plan.currency, plan.billing_interval, plan.base_amount_cents, plan.base_bill_timing, plan.meter_ids])
    .toEqual([planName, planCode, rule.currency, 'monthly', 2900, 'in_arrears', [meter.id]])
  const planRow = page.getByRole('row').filter({ hasText: planCode })
  await expect(planRow.getByRole('cell')).toHaveText([planName, planCode, 'monthly', 'active', usd(plan.base_amount_cents), '1'])

  // The plan's page shows the same fee, and the meter with its price.
  await planRow.getByRole('link', { name: planName }).click()
  await expect(page.getByRole('heading', { level: 1, name: planName, exact: true })).toBeVisible()
  await expect(field(page.getByRole('main'), 'Base Price')).toContainText(usd(plan.base_amount_cents))
  const meters = card(page, 'Meters (1)')
  await expect(meters).toContainText(meterName)
  await expect(meters).toContainText(`${priceName} — $0.002 / call`)
})

// The upcoming-invoice card on the subscription page lists the lines the API
// previews, in order, with their amounts and the total.
async function expectUpcomingInvoiceMatchesAPI(page: Page, subscriptionId: string): Promise<Preview> {
  const preview = await api<Preview>('GET', `/billing/preview/${subscriptionId}`)
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

// Moves a clock just past the end of its first monthly period.
async function advancePastFirstPeriod(clock: TestClock) {
  const to = new Date(clock.frozen_time)
  to.setUTCMonth(to.getUTCMonth() + 1)
  to.setUTCDate(to.getUTCDate() + 1)
  await api('POST', `/test-clocks/${clock.id}/advance`, { frozen_time: to.toISOString().replace(/\.\d+Z$/, 'Z') })
  await waitForClockReady(clock.id)
}

test('a price override for one customer: the upcoming invoice bills the negotiated rate while it lasts, and the list rate once it ends', async ({ page }) => {
  const id = uniq()
  const rule = await api<RatingRule>('POST', '/rating-rules', {
    rule_key: `e2e_deal_${id}`, name: `E2E Deal price ${id}`, mode: 'flat', currency: 'USD', flat_amount_cents: '2',
  })
  const meter = await api<Meter>('POST', '/meters', {
    key: `e2e_deal_${id}`, name: `E2E Deal calls ${id}`, unit: 'call', aggregation: 'sum', rating_rule_version_id: rule.id,
  })
  const plan = await api<Plan>('POST', '/plans', {
    code: `e2e-deal-${id}`, name: `E2E Deal plan ${id}`, currency: 'USD', billing_interval: 'monthly', base_amount_cents: 0, meter_ids: [meter.id],
  })
  const clock = await createClock()
  const customer = await createCustomer(clock.id)
  const subscription = await createSubscription(customer.id, plan.id)
  // An override applies from the first period that opens after it is made.
  // On a test clock moved past real time, that is the period in progress:
  // the dialog says so, and it lets this journey see the change at once.
  await advancePastFirstPeriod(clock)
  await api('POST', '/usage-events', { external_customer_id: customer.external_id, event_name: meter.key, quantity: '500', idempotency_key: `e2e_${uniq()}` })

  // Control: the list rate, 2 cents a call.
  expect(usageRate(await expectUpcomingInvoiceMatchesAPI(page, subscription.id))).toEqual([2])

  // Add a negotiated rate of $0.01.
  const reason = `E2E annual deal ${id}`
  await page.goto(`/customers/${customer.id}`)
  const overrides = card(page, 'Price overrides')
  await expect(overrides).toContainText('No negotiated prices — this customer bills at list price.')
  await overrides.getByRole('button', { name: 'Add override' }).click()
  const add = page.getByRole('dialog', { name: 'Add price override' })
  await add.getByRole('button', { name: 'Select a price...' }).click()
  await pick(page, rule.rule_key, new RegExp(`^${escapeRegExp(`${rule.name} (${rule.rule_key}) — list $0.02/unit`)}`))
  await field(add, /^Negotiated price per unit/).getByRole('spinbutton').fill('0.01')
  await expect(add.getByText('List price: $0.02/unit.', { exact: false })).toBeVisible()
  await field(add, 'Reason (optional)').getByRole('textbox').fill(reason)
  await expect(add.getByText('Test-clock customer: in simulations, price changes apply to the current simulated period too', { exact: false })).toBeVisible()
  const saving = apiCall(page, 'POST', /\/price-overrides$/)
  await add.getByRole('button', { name: 'Save override' }).click()
  const savedRes = await saving
  expect(savedRes.status()).toBe(201)
  const saved = (await savedRes.json()) as PriceOverride
  expect([saved.rule_key, saved.flat_amount_cents, saved.active]).toEqual([rule.rule_key, '1', true])
  await expect(add).toBeHidden()
  const active = async () => (await api<{ data: PriceOverride[] }>('GET', `/price-overrides?customer_id=${customer.id}`)).data.filter(o => o.active)
  expect((await active()).map(o => o.id)).toEqual([saved.id])
  await expect(overrides).toContainText(rule.name)
  await expect(overrides).toContainText(rule.rule_key)
  await expect(overrides).toContainText('$0.01/unit · list price $0.02/unit')
  await expect(overrides).toContainText(`${reason} · since`)

  // The upcoming invoice now bills the usage at the negotiated rate.
  expect(usageRate(await expectUpcomingInvoiceMatchesAPI(page, subscription.id))).toEqual([1])

  // End it: the card is empty again and the list rate is back.
  await page.goto(`/customers/${customer.id}`)
  await overrides.getByRole('button', { name: `End override on ${rule.name}` }).click()
  const end = page.getByRole('dialog', { name: 'End price override' })
  await expect(end).toContainText(`${rule.name} returns to list price for this customer.`)
  const ending = apiCall(page, 'DELETE', new RegExp(`/price-overrides/${saved.id}$`))
  await end.getByRole('button', { name: 'End override' }).click()
  expect((await ending).ok()).toBe(true)
  await expect(end).toBeHidden()
  expect(await active()).toEqual([])
  await expect(overrides).toContainText('No negotiated prices — this customer bills at list price.')

  expect(usageRate(await expectUpcomingInvoiceMatchesAPI(page, subscription.id))).toEqual([2])
})

// Installing a recipe can take over the default dunning policy (ADR-123),
// and the dunning journey moves the default and puts it back. They run one
// after the other, so neither sees the other's change half done.
test.describe('the default dunning policy', () => {
  test.describe.configure({ mode: 'default' })

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
      await expect(section('Prices')).toHaveText(objects.rating_rules.map(r => new RegExp(`^· ${escapeRegExp(r.name || r.key)} — `)))
      await expect(section('Plan')).toHaveText(objects.plans.map(p =>
        `· ${p.name} — ${p.billing_interval}, ${p.currency}${p.base_amount_cents > 0 ? `, ${usd(p.base_amount_cents)} base` : ''}`))
      await expect(section('Payment retries')).toHaveText(objects.dunning_policies.map(d => new RegExp(`^· ${escapeRegExp(d.name)} — up to ${d.max_retries} retries`)))
      await expect(section('Notifications')).toHaveText(objects.webhook_endpoints.map(w => `· ${w.url_placeholder} — set your real URL after install`))

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
      expect([plan.name, plan.code, plan.meter_ids]).toEqual([planName, planCode, instance.created_objects.meter_ids])
      await expect(page.getByRole('heading', { level: 1, name: planName, exact: true })).toBeVisible()
      const meters = card(page, 'Meters (1)')
      await expect(meters).toContainText(objects.meters[0].name)
      await expect(meters).toContainText(`${instance.created_objects.pricing_rule_ids?.length} dimension prices`)
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
    await installed.getByRole('button', { name: 'View' }).click()
    await expect(dialog.getByText('Installed with these settings', { exact: true })).toBeVisible()
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
    expect(meters.filter(m => m.key === 'gpu_seconds').map(m => m.id)).toEqual(instance.created_objects.meter_ids)
  })

  test('dunning policies: create one turned off, turn it on and edit it, give it to a customer (it cannot be deleted then), and make it the default', async ({ page }) => {
    const id = uniq()
    const name = `E2E retries ${id}`
    const customer = await createCustomer()
    const policies = async () => (await api<{ data: DunningPolicy[] }>('GET', '/dunning/policies')).data
    const policy = async (policyId: string) => (await policies()).find(p => p.id === policyId)
    // A policy's row: the innermost block holding both its name and its
    // Edit button.
    const policyRow = (policyName: string) => page.getByRole('main').locator('div')
      .filter({ has: page.getByText(policyName, { exact: true }) })
      .filter({ has: page.getByRole('button', { name: `Edit policy ${policyName}`, exact: true }) })
      .last()
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
      await field(create, 'Grace (days)').getByRole('spinbutton').fill('2')
      await field(create, 'Do this to the subscription').getByRole('combobox').click()
      await page.getByRole('option', { name: 'Leave it active' }).click()
      await expect(create.getByText(/^The subscription keeps billing as normal\. The unpaid invoice stays open and due\./)).toBeVisible()
      const creating = apiCall(page, 'POST', /\/dunning\/policies$/)
      await create.getByRole('button', { name: 'Save' }).click()
      const createdRes = await creating
      expect(createdRes.status()).toBe(201)
      const created = (await createdRes.json()) as DunningPolicy
      expect(created).toMatchObject({
        name, enabled: false, is_default: false, max_retry_attempts: 3, grace_period_days: 2,
        retry_schedule: ['72h', '120h'], final_subscription_action: 'none', final_invoice_action: 'none',
      })
      await expect(toasts(page).getByText('Policy created', { exact: true })).toBeVisible()
      const row = policyRow(name)
      for (const text of ['disabled', '0 assigned', 'Max retries: 3', 'Grace: 2d', 'Schedule: 72h · 120h', 'On exhaustion: no action']) {
        await expect(row).toContainText(text)
      }

      // Turn it on with four retries: the dialog asks for a third interval
      // before the schedule covers them.
      await row.getByRole('button', { name: `Edit policy ${name}` }).click()
      const edit = page.getByRole('dialog', { name: 'Edit policy' })
      await edit.getByRole('switch').click()
      await expect(edit.getByRole('switch')).toBeChecked()
      await field(edit, 'Max retry attempts').getByRole('spinbutton').fill('4')
      await expect(edit.getByText('Missing 1 entries.', { exact: false })).toBeVisible()
      await field(edit, 'Retry schedule').getByRole('textbox').fill('72h, 120h, 168h')
      await expect(edit.getByText(/Missing \d+ entries/)).toHaveCount(0)
      const saving = apiCall(page, 'PATCH', new RegExp(`/dunning/policies/${created.id}$`))
      await edit.getByRole('button', { name: 'Save' }).click()
      expect((await saving).ok()).toBe(true)
      await expect(toasts(page).getByText('Policy saved', { exact: true })).toBeVisible()
      expect(await policy(created.id)).toMatchObject({
        enabled: true, is_default: false, max_retry_attempts: 4, grace_period_days: 2, retry_schedule: ['72h', '120h', '168h'],
      })
      await expect(row).not.toContainText('disabled')
      await expect(row).toContainText('Max retries: 4')
      await expect(row).toContainText('Schedule: 72h · 120h · 168h')

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
      await expect(field(dunning, 'Max retry attempts')).toContainText('4')
      await expect(field(dunning, 'Retry schedule')).toContainText('72h · 120h · 168h')

      // While a customer has it, the policy cannot be deleted: the page says
      // why and asks nothing.
      await page.goto('/dunning-policies')
      expect((await policy(created.id))?.assigned_customers).toBe(1)
      await expect(row).toContainText('1 assigned')
      await row.getByRole('button', { name: `Delete policy ${name}` }).click()
      await expect(toasts(page).getByText('1 customer(s) assigned — reassign them first', { exact: true })).toBeVisible()
      await expect(page.getByRole('alertdialog')).toHaveCount(0)
      expect(await policy(created.id)).toBeDefined()

      // Make it the default: the badge moves from the old default to it.
      const previous = (await policies()).find(p => p.is_default)
      restoreDefault = previous?.id
      const settingDefault = apiCall(page, 'POST', new RegExp(`/dunning/policies/${created.id}/set-default$`))
      await row.getByRole('button', { name: 'Make default' }).click()
      expect((await settingDefault).ok()).toBe(true)
      await expect(toasts(page).getByText('Default policy updated', { exact: true })).toBeVisible()
      expect((await policies()).filter(p => p.is_default).map(p => p.id)).toEqual([created.id])
      await expect(row.getByText('default', { exact: true })).toBeVisible()
      await expect(row.getByRole('button', { name: 'Make default' })).toHaveCount(0)
      await expect(row.getByRole('button', { name: `Delete policy ${name}` })).toHaveCount(0)
      if (previous?.name) {
        const old = policyRow(previous.name)
        await expect(old.getByText('default', { exact: true })).toHaveCount(0)
        await expect(old.getByRole('button', { name: 'Make default' })).toBeVisible()
      }
    } finally {
      // Every test shares this workspace's default: put the old one back.
      if (restoreDefault) await api('POST', `/dunning/policies/${restoreDefault}/set-default`)
    }
  })
})
