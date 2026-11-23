import { expect } from '@playwright/test'
import { api, uniq } from './api'

// The few API shapes the suite reads. Only the fields used here are listed.
export interface Customer { id: string; display_name: string; external_id: string; email: string }
export interface Plan { id: string; name: string; code: string }
export interface Meter { id: string; name: string; event_name: string }
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

export function createPlan(currency = 'USD'): Promise<Plan> {
  const id = uniq()
  return api<Plan>('POST', '/plans', {
    code: `e2e-plan-${id}`,
    name: `E2E Plan ${id}`,
    currency,
    billing_interval: 'monthly',
    base_amount_cents: 4900,
    base_bill_timing: 'in_arrears',
  })
}

export function createMeter(): Promise<Meter> {
  const id = uniq()
  return api<Meter>('POST', '/meters', { event_name: `e2e_calls_${id}`, name: `E2E Calls ${id}`, unit: 'call', aggregation: 'sum' })
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

// A customer with one open invoice: 49 calls at $1.00 on a plan with no base
// fee, then the subscription canceled, which issues its final invoice at once.
// It bills usage because a same-day cancel bills an in-arrears base fee only
// after noon (a period's days round to whole days).
export async function billedCustomer() {
  const id = uniq()
  const plan = await api<Plan>('POST', '/plans', {
    code: `e2e-usage-plan-${id}`,
    name: `E2E Usage Plan ${id}`,
    currency: 'USD',
    billing_interval: 'monthly',
    base_amount_cents: 0,
    base_bill_timing: 'in_arrears',
  })
  const meter = await createMeter()
  await api('POST', `/plans/${plan.id}/prices`, { meter_id: meter.id, mode: 'per_unit', unit_amount_cents: '100' })
  const customer = await createCustomer()
  const subscription = await createSubscription(customer.id, plan.id)
  await api('POST', '/usage-events', { external_customer_id: customer.external_id, event_name: meter.event_name, quantity: '49', idempotency_key: `e2e_${uniq()}` })
  await api('POST', `/subscriptions/${subscription.id}/cancel`)
  const invoices = await invoicesFor(customer.id)
  if (invoices.length !== 1) throw new Error(`expected the cancel's final invoice, got ${invoices.length} invoices`)
  const invoice = await api<{ invoice: Invoice }>('GET', `/invoices/${invoices[0].id}`)
  return { plan, customer, subscription, invoice: invoice.invoice }
}

// Everything the page smoke test needs so that no page renders empty: a billed
// customer with a live subscription, a meter, a webhook endpoint, and a credit
// grant to another customer. The billed customer holds no credit, so its open
// invoice stays as billed: the auto-charge sweep draws credit onto an open
// invoice whenever it next runs.
export async function seedWorld() {
  const billed = await billedCustomer()
  const subscription = await createSubscription(billed.customer.id, billed.plan.id)
  const creditCustomer = await createCustomer()
  const meter = await createMeter()
  const webhook = await api<{ endpoint: { id: string } }>('POST', '/webhook-endpoints/endpoints', {
    url: 'https://example.com/hardcount-e2e',
    description: 'E2E endpoint',
  })
  await api('POST', '/credits/grant', { customer_id: creditCustomer.id, currency: 'USD', amount_cents: 1000, description: 'E2E welcome credit' })
  return {
    customer: billed.customer,
    creditCustomer,
    plan: billed.plan,
    subscription,
    invoice: billed.invoice,
    meter,
    webhookEndpointId: webhook.endpoint.id,
  }
}

export type Seed = Awaited<ReturnType<typeof seedWorld>>
