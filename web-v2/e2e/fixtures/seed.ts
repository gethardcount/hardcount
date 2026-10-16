import { expect } from '@playwright/test'
import { api, uniq } from './api'

// The few API shapes the suite reads. Only the fields used here are listed.
export interface Customer { id: string; display_name: string; external_id: string }
export interface Plan { id: string; name: string; code: string }
export interface Meter { id: string; name: string }
export interface TestClock { id: string; name: string; frozen_time: string; status: string }
export interface Subscription { id: string; display_name: string; code: string; status: string }
export interface Invoice {
  id: string
  invoice_number: string
  status: string
  subtotal_cents: number
  total_amount_cents: number
  amount_due_cents: number
  public_token?: string
}

export function createPlan(): Promise<Plan> {
  const id = uniq()
  return api<Plan>('POST', '/plans', {
    code: `e2e-plan-${id}`,
    name: `E2E Plan ${id}`,
    currency: 'USD',
    billing_interval: 'monthly',
    base_amount_cents: 4900,
  })
}

export function createMeter(): Promise<Meter> {
  const id = uniq()
  return api<Meter>('POST', '/meters', { key: `e2e_calls_${id}`, name: `E2E Calls ${id}`, unit: 'call', aggregation: 'sum' })
}

// A test clock frozen at the current time, so billing periods line up with
// what an operator would see today.
export function createClock(): Promise<TestClock> {
  return api<TestClock>('POST', '/test-clocks', {
    name: `E2E Clock ${uniq()}`,
    frozen_time: new Date().toISOString().replace(/\.\d+Z$/, 'Z'),
  })
}

export function createCustomer(testClockId?: string): Promise<Customer> {
  const id = uniq()
  return api<Customer>('POST', '/customers', {
    external_id: `e2e_${id}`,
    display_name: `E2E Customer ${id}`,
    email: `e2e-${id}@example.com`,
    ...(testClockId ? { test_clock_id: testClockId } : {}),
  })
}

export function createSubscription(customerId: string, planId: string): Promise<Subscription> {
  const id = uniq()
  return api<Subscription>('POST', '/subscriptions', {
    code: `e2e-sub-${id}`,
    display_name: `E2E Subscription ${id}`,
    customer_id: customerId,
    items: [{ plan_id: planId, quantity: 1 }],
    billing_time: 'anniversary',
    start_now: true,
  })
}

// Waits until a clock has finished its billing catchup.
export async function waitForClockReady(clockId: string): Promise<TestClock> {
  let clock: TestClock | undefined
  await expect
    .poll(async () => (clock = await api<TestClock>('GET', `/test-clocks/${clockId}`)).status, { timeout: 30_000 })
    .toBe('ready')
  return clock!
}

export async function invoicesFor(customerId: string): Promise<Invoice[]> {
  const res = await api<{ data: Invoice[] }>('GET', `/invoices?customer_id=${customerId}`)
  return res.data
}

// A customer on its own test clock with a monthly subscription, not yet billed.
export async function subscribedCustomer() {
  const plan = await createPlan()
  const clock = await createClock()
  const customer = await createCustomer(clock.id)
  const subscription = await createSubscription(customer.id, plan.id)
  return { plan, clock, customer, subscription }
}

// The same, with the clock moved past the first renewal so one invoice exists.
export async function billedCustomer() {
  const world = await subscribedCustomer()
  const to = new Date(world.clock.frozen_time)
  to.setUTCMonth(to.getUTCMonth() + 1)
  to.setUTCDate(to.getUTCDate() + 1)
  await api('POST', `/test-clocks/${world.clock.id}/advance`, { frozen_time: to.toISOString().replace(/\.\d+Z$/, 'Z') })
  await waitForClockReady(world.clock.id)
  const invoices = await invoicesFor(world.customer.id)
  if (invoices.length !== 1) throw new Error(`expected 1 invoice after the renewal, got ${invoices.length}`)
  const invoice = await api<{ invoice: Invoice }>('GET', `/invoices/${invoices[0].id}`)
  return { ...world, invoice: invoice.invoice }
}

// Everything the page smoke test needs so that no page renders empty: a billed
// customer, a meter, a webhook endpoint and a credit grant.
export async function seedWorld() {
  const billed = await billedCustomer()
  const meter = await createMeter()
  const webhook = await api<{ endpoint: { id: string } }>('POST', '/webhook-endpoints/endpoints', {
    url: 'https://example.com/velox-e2e',
    description: 'E2E endpoint',
  })
  await api('POST', '/credits/grant', { customer_id: billed.customer.id, amount_cents: 1000, description: 'E2E welcome credit' })
  return {
    customer: billed.customer,
    plan: billed.plan,
    clock: billed.clock,
    subscription: billed.subscription,
    invoice: billed.invoice,
    meter,
    webhookEndpointId: webhook.endpoint.id,
  }
}

export type Seed = Awaited<ReturnType<typeof seedWorld>>
