import { useSyncExternalStore } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { useAuth } from '@/contexts/AuthContext'
import { setupChecklistState } from '@/lib/homeNotices'

export interface OnboardingStep {
  key: 'stripe' | 'plan' | 'customer' | 'subscription' | 'usage' | 'webhook'
  label: string
  desc: string
  to: string
  cta: string
  done: boolean
}

// Per-tenant so a user admin-ing multiple tenants doesn't mute the launcher
// globally. The account menu's "Show setup checklist" un-dismisses.
const dismissStorageKey = (tenantID: string) => `velox:getstarted-dismissed:${tenantID}`

function readDismissed(tenantID: string): boolean {
  try {
    return localStorage.getItem(dismissStorageKey(tenantID)) === '1'
  } catch {
    // private-mode Safari throws on localStorage access — treat as not-dismissed
    return false
  }
}

// Hiding and showing the checklist happen in different components (the
// panel's X, the account menu's "Show setup checklist"), each with its own
// copy of this hook. A change event tells every copy to re-read storage.
const DISMISS_EVENT = 'velox:getstarted-dismissed'

function subscribeDismissed(onChange: () => void): () => void {
  window.addEventListener(DISMISS_EVENT, onChange)
  window.addEventListener('storage', onChange)
  return () => {
    window.removeEventListener(DISMISS_EVENT, onChange)
    window.removeEventListener('storage', onChange)
  }
}

// The checklist panel is rendered once by Layout; the sidebar row and the
// Home welcome card open it with this event.
const OPEN_EVENT = 'velox:open-setup-checklist'

export function openSetupChecklist() {
  window.dispatchEvent(new Event(OPEN_EVENT))
}

export function onOpenSetupChecklist(handler: () => void): () => void {
  window.addEventListener(OPEN_EVENT, handler)
  return () => window.removeEventListener(OPEN_EVENT, handler)
}

// Home opens the checklist by itself once per user, the first time a new
// account (no subscriptions yet) lands there. Once is remembered in this
// browser. When storage can't be read we say "already shown": a checklist
// that pops open on every visit is worse than one that never does.
const autoOpenedKey = (userID: string) => `velox:setup-checklist-auto-opened:${userID}`

export function claimChecklistAutoOpen(userID: string): boolean {
  try {
    if (localStorage.getItem(autoOpenedKey(userID)) === '1') return false
    localStorage.setItem(autoOpenedKey(userID), '1')
    return true
  } catch {
    return false
  }
}

export function useOnboardingSteps() {
  const { user } = useAuth()
  const dismissed = useSyncExternalStore(
    subscribeDismissed,
    () => (user ? readDismissed(user.tenant_id) : false),
  )

  // setDismissed(true) hides the checklist; setDismissed(false) shows it again.
  const setDismissed = (hidden = true) => {
    if (!user) return
    try {
      if (hidden) localStorage.setItem(dismissStorageKey(user.tenant_id), '1')
      else localStorage.removeItem(dismissStorageKey(user.tenant_id))
    } catch {
      // see above
    }
    window.dispatchEvent(new Event(DISMISS_EVENT))
  }

  // Stripe creds are queried even when the launcher is dismissed because the
  // live-mode hard blocker (rendered in Layout) depends on the same signal.
  const { data: stripeCreds } = useQuery({
    queryKey: ['onboarding-stripe-creds'],
    queryFn: () => api.listStripeCredentials(),
    enabled: !!user,
  })

  // Queried while hidden too: the account menu offers "Show setup checklist"
  // only when a step is left (setupChecklistState), which needs the steps.
  const enabled = !!user
  const { data: overview } = useQuery({
    queryKey: ['onboarding-overview'],
    queryFn: () => api.getAnalyticsOverview(),
    enabled,
  })
  const { data: plansList } = useQuery({
    queryKey: ['onboarding-plans'],
    queryFn: () => api.listPlans(),
    enabled,
  })
  const { data: webhookEndpoints } = useQuery({
    queryKey: ['onboarding-webhook-endpoints'],
    queryFn: () => api.listWebhookEndpoints(),
    enabled,
  })
  // Any subscription at all, in any state and on any customer (test-clock
  // ones too). The overview's active_subscriptions leaves out trialing and
  // test-clock subscriptions, so a first subscription made either way left
  // the step undone. Home's welcome card reads the same answer.
  const { data: anySubscription } = useQuery({
    queryKey: ['onboarding-any-subscription'],
    queryFn: () => api.listSubscriptions('limit=1'),
    enabled,
  })
  // Usage: the overview already counts events in the last 30 days. Only when
  // that is 0 do we ask whether any event exists at all, so a busy account
  // never pays for the second request. Same key and question as the Usage
  // page's empty state.
  const usageInWindow = overview?.usage_events ?? 0
  const { data: anyUsageEver, isError: anyUsageFailed } = useQuery({
    queryKey: ['usage-events-any-ever'],
    queryFn: async () => ((await api.listUsageEvents('limit=1')).data ?? []).length > 0,
    enabled: enabled && !!overview && usageInWindow === 0,
    retry: false,
  })

  const hasAnyStripe = (stripeCreds?.data?.length ?? 0) > 0
  // Tri-state: undefined = still loading. Callers (live-mode blocker strip)
  // must wait for a definitive false before raising an alert.
  const hasLiveStripe: boolean | undefined = stripeCreds
    ? stripeCreds.data.some(c => c.livemode)
    : undefined
  const hasPlans = (plansList?.data?.length ?? 0) > 0
  const hasWebhook = (webhookEndpoints?.data?.length ?? 0) > 0
  const hasCustomer = (overview?.active_customers ?? 0) > 0
  const hasSubscription = (anySubscription?.data?.length ?? 0) > 0
  const hasUsage = usageInWindow > 0 || anyUsageEver === true
  // undefined while loading: Home must not flash the welcome card at an
  // account that has subscriptions.
  const newAccount: boolean | undefined = anySubscription ? !hasSubscription : undefined

  const steps: OnboardingStep[] = [
    {
      key: 'stripe',
      label: 'Connect Stripe',
      desc: "Needed to charge your customers' cards",
      to: '/settings?tab=payments',
      cta: 'Connect Stripe',
      done: hasAnyStripe,
    },
    {
      key: 'plan',
      label: 'Create your first plan',
      desc: 'Set what you charge: a base fee, usage prices, or both',
      to: '/pricing',
      cta: 'Set up pricing',
      done: hasPlans,
    },
    {
      key: 'customer',
      label: 'Add your first customer',
      desc: 'Who are you billing?',
      to: '/customers',
      cta: 'Add a customer',
      done: hasCustomer,
    },
    {
      key: 'subscription',
      label: 'Create a subscription',
      desc: 'Subscribe the customer to a plan',
      to: '/subscriptions',
      cta: 'New subscription',
      done: hasSubscription,
    },
    {
      key: 'usage',
      label: 'Send a usage event',
      desc: 'Your app reports what each customer used; Velox prices it.',
      to: '/usage',
      cta: 'Go to Usage',
      done: hasUsage,
    },
    {
      key: 'webhook',
      label: 'Set up a webhook endpoint',
      desc: 'Let your app hear about invoices and subscriptions',
      to: '/webhooks',
      cta: 'Add endpoint',
      done: hasWebhook,
    },
  ]

  const complete = steps.filter(s => s.done).length
  const total = steps.length
  // Don't flash the launcher before queries settle — avoids "0 of 5" pills
  // flickering for users who've actually completed everything.
  // A failed "any usage ever" check counts as settled (step not done) so it
  // can't hold the whole checklist back.
  const usageSettled = usageInWindow > 0 || anyUsageEver !== undefined || anyUsageFailed
  const loaded = !!overview && !!plansList && !!stripeCreds && !!webhookEndpoints && !!anySubscription && usageSettled
  const { showPill: show, offerShowAgain } = setupChecklistState(dismissed, loaded, complete, total)

  return {
    steps,
    complete,
    total,
    dismissed,
    setDismissed,
    show,
    offerShowAgain,
    hasLiveStripe,
    newAccount,
  }
}
