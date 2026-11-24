// The setup list: the operator's job, in order, from connecting Stripe to
// selling a customer credit. useOnboardingSteps reads the facts; these plain
// functions turn them into steps, so the unit tests can check each one.
import type { StripeKeyState } from '@/lib/stripeVerify'

export type SetupStepKey = 'stripe' | 'key' | 'prices' | 'costs' | 'subscription' | 'usage' | 'credit'

export interface SetupStepAction {
  label: string
  to: string
}

export interface SetupStep {
  key: SetupStepKey
  label: string
  desc: string
  // Shown in its place but not counted: the list is finished without it.
  optional: boolean
  // One action makes the whole row a link; two are offered side by side.
  actions: SetupStepAction[]
  done: boolean
}

// What the account has in the mode the operator is in.
export interface SetupFacts {
  stripe: StripeKeyState | undefined
  gatewayKey: boolean
  pricedPlan: boolean
  providerCost: boolean
  subscription: boolean
  usage: boolean
  credit: boolean
}

export function setupSteps(f: SetupFacts): SetupStep[] {
  const refused = f.stripe === 'refused'
  return [
    {
      key: 'stripe',
      label: 'Connect Stripe',
      desc: refused
        ? 'Stripe refused the saved key. Paste the current one from your Stripe dashboard.'
        : "Needed to charge your customers' cards",
      optional: false,
      actions: [{ label: refused ? 'Replace key' : 'Connect Stripe', to: '/settings?tab=payments' }],
      done: f.stripe === 'connected',
    },
    {
      key: 'key',
      label: 'Make a key for your gateway',
      desc: 'A usage-only key can only send usage. Put it in LiteLLM or your own gateway.',
      optional: false,
      actions: [{ label: 'Make a key', to: '/api-keys' }],
      done: f.gatewayKey,
    },
    {
      key: 'prices',
      label: 'Set your prices',
      desc: 'What you charge for each model and token type, on a plan',
      optional: false,
      actions: [
        { label: 'Start from a recipe', to: '/pricing?tab=recipes' },
        { label: 'Set them yourself', to: '/pricing' },
      ],
      done: f.pricedPlan,
    },
    {
      key: 'costs',
      label: 'Record what providers charge you',
      desc: "For your margin. Record them before usage arrives: Hardcount sets each event's cost when it arrives.",
      optional: true,
      actions: [{ label: 'Add provider costs', to: '/pricing?tab=provider-costs' }],
      done: f.providerCost,
    },
    {
      key: 'subscription',
      label: 'Add a customer and put them on a plan',
      desc: 'Add who you bill, then put them on a plan from their page',
      optional: false,
      actions: [{ label: 'Add a customer', to: '/customers' }],
      done: f.subscription,
    },
    {
      key: 'usage',
      label: 'Send usage from your gateway',
      desc: 'Your gateway reports what each customer used, and Hardcount prices it',
      optional: false,
      actions: [{ label: 'Go to Usage', to: '/usage' }],
      done: f.usage,
    },
    {
      key: 'credit',
      label: 'Sell a customer credit',
      desc: 'Prepaid credit pays their invoices before their card is charged',
      optional: false,
      actions: [{ label: 'Sell credit', to: '/credits' }],
      done: f.credit,
    },
  ]
}

// The count the sidebar row and the list show. Optional steps are left out,
// so skipping one still finishes the list.
export function setupProgress(steps: readonly SetupStep[]): { complete: number; total: number } {
  const required = steps.filter(s => !s.optional)
  return { complete: required.filter(s => s.done).length, total: required.length }
}

// A key a gateway sends usage with: an active usage-only key. The keys every
// account is created with are secret keys, and the key list does not say who
// made a key, so a usage-only key is the one sign that the operator made one.
export function isGatewayKey(key: { key_type: string; status: string }): boolean {
  return key.key_type === 'ingest' && key.status === 'active'
}

// Whether any plan has a usage price. A plan's prices are its usage prices;
// its base fee is apart from them.
export function hasPricedPlan(plans: readonly { prices?: readonly unknown[] | null }[]): boolean {
  return plans.some(p => (p.prices?.length ?? 0) > 0)
}
