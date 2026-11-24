import { useSyncExternalStore } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { listCreditBalances, listProviderCosts } from '@/lib/gen/queries.gen'
import { useAuth } from '@/contexts/AuthContext'
import { setupChecklistState } from '@/lib/homeNotices'
import { hasPricedPlan, isGatewayKey, setupProgress, setupSteps, type SetupStepKey } from '@/lib/setupSteps'
import { stripeKeyState, type StripeKeyState } from '@/lib/stripeVerify'

// Per-tenant so a user admin-ing multiple tenants doesn't mute the launcher
// globally. The account menu's "Show setup checklist" un-dismisses.
const dismissStorageKey = (tenantID: string) => `hardcount:getstarted-dismissed:${tenantID}`

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
const DISMISS_EVENT = 'hardcount:getstarted-dismissed'

function subscribeDismissed(onChange: () => void): () => void {
  window.addEventListener(DISMISS_EVENT, onChange)
  window.addEventListener('storage', onChange)
  return () => {
    window.removeEventListener(DISMISS_EVENT, onChange)
    window.removeEventListener('storage', onChange)
  }
}

// The checklist panel is rendered once by Layout; the sidebar row opens it
// with this event. Home shows the list in the page instead (SetupChecklistCard).
const OPEN_EVENT = 'hardcount:open-setup-checklist'

export function openSetupChecklist() {
  window.dispatchEvent(new Event(OPEN_EVENT))
}

export function onOpenSetupChecklist(handler: () => void): () => void {
  window.addEventListener(OPEN_EVENT, handler)
  return () => window.removeEventListener(OPEN_EVENT, handler)
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
  // Same key as Settings and the invoice page, so connecting or replacing a
  // key in Settings refreshes the step and the warning at once.
  const { data: stripeCreds } = useQuery({
    queryKey: ['stripe-credentials'],
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
  // The gateway key step (isGatewayKey). Keys are few, so the whole list.
  const { data: apiKeys } = useQuery({
    queryKey: ['onboarding-api-keys'],
    queryFn: () => api.listApiKeys(),
    enabled,
  })
  // One provider cost is enough to tick its step.
  const { data: providerCosts } = useQuery({
    queryKey: ['onboarding-provider-costs'],
    queryFn: () => listProviderCosts({ limit: 1 }),
    enabled,
  })
  // Any customer who was ever given or sold credit: the balances list has a
  // row for each customer with a credit record, used up or not, so the step
  // stays done after the credit is spent.
  const { data: creditBalances } = useQuery({
    queryKey: ['onboarding-any-credit'],
    queryFn: () => listCreditBalances({ limit: 1 }),
    enabled,
  })
  // Any subscription at all, in any state, so a first subscription on a
  // trial counts. Home's welcome card reads the same answer.
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

  // The step asks about the mode the operator is in: a test key doesn't
  // connect live mode, and a key Stripe refused connects nothing.
  const stripeState: StripeKeyState | undefined = stripeCreds && user
    ? stripeKeyState(stripeCreds.data, user.livemode)
    : undefined
  // undefined = still loading. The live-mode warning waits for an answer
  // before it shows.
  const liveStripe: StripeKeyState | undefined = stripeCreds
    ? stripeKeyState(stripeCreds.data, true)
    : undefined
  const hasSubscription = (anySubscription?.data?.length ?? 0) > 0
  // undefined while loading: Home must not flash the welcome card at an
  // account that has subscriptions.
  const newAccount: boolean | undefined = anySubscription ? !hasSubscription : undefined

  const steps = setupSteps({
    stripe: stripeState,
    gatewayKey: (apiKeys?.data ?? []).some(isGatewayKey),
    pricedPlan: hasPricedPlan(plansList?.data ?? []),
    providerCost: (providerCosts?.data.length ?? 0) > 0,
    subscription: hasSubscription,
    usage: usageInWindow > 0 || anyUsageEver === true,
    credit: (creditBalances?.total ?? 0) > 0,
  })

  const { complete, total } = setupProgress(steps)
  // Don't flash the launcher before queries settle — avoids a "0 of N" row
  // flickering for users who've actually completed everything.
  // A failed "any usage ever" check counts as settled (step not done) so it
  // can't hold the whole checklist back.
  const usageSettled = usageInWindow > 0 || anyUsageEver !== undefined || anyUsageFailed
  const loaded = !!overview && !!plansList && !!stripeCreds && !!apiKeys && !!providerCosts && !!anySubscription && !!creditBalances && usageSettled
  const { showPill: show, offerShowAgain } = setupChecklistState(dismissed, loaded, complete, total)

  // stepLeft is true once the step is known not to be done. A page offers the
  // next step only then, so it never flashes the offer while the list loads.
  const stepLeft = (key: SetupStepKey): boolean =>
    loaded && steps.some(s => s.key === key && !s.done)

  return {
    steps,
    complete,
    total,
    loaded,
    dismissed,
    setDismissed,
    show,
    offerShowAgain,
    stepLeft,
    liveStripe,
    newAccount,
  }
}
