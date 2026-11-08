import { useQuery } from '@tanstack/react-query'
import { api, ApiError } from '@/lib/api'
import { profileOrAbsent } from '@/lib/billingProfileLoad'
import { settledCustomerCurrency, type CustomerCurrency } from '@/lib/customerCurrency'
import { useTenantSettings } from '@/hooks/useTenantSettings'

// useCustomerSubscriptions is every subscription of one customer, filtered on
// the server. The keys are shared with the customer page's other cards, so a
// page makes each request once.
export function useCustomerSubscriptions(customerId: string | undefined) {
  return useQuery({
    queryKey: ['customer-subs-upcoming', customerId],
    queryFn: () => api.listSubscriptions(`customer_id=${encodeURIComponent(customerId!)}&limit=100`),
    enabled: !!customerId,
  })
}

// CustomerCurrencyRead is the customer's currency and, while it is unknown,
// why. With no currency and no failure, a read behind it is still loading.
export interface CustomerCurrencyRead {
  currency?: CustomerCurrency
  // Set when a read the answer needs has failed. The currency stays unknown
  // until retry loads the failed reads again.
  failure?: { error: unknown; retry: () => void }
}

// useCustomerCurrency is resolveCustomerCurrency fed from the server: the
// customer's subscriptions, every plan (an archived plan can still bill a live
// subscription), the billing profile, the recent invoices and the account
// settings (whose default currency is the last answer). The currency is
// unknown until the customer's reads load, and stays unknown if one of them
// fails (settledCustomerCurrency says why). The account settings matter only
// for a customer with no currency of their own.
export function useCustomerCurrency(customerId: string | undefined): CustomerCurrencyRead {
  const subs = useCustomerSubscriptions(customerId)
  const plans = useQuery({
    queryKey: ['plans'],
    queryFn: () => api.listPlans(),
  })
  const profile = useQuery({
    queryKey: ['customer-billing-profile', customerId],
    queryFn: () => profileOrAbsent(
      () => api.getBillingProfile(customerId!),
      err => err instanceof ApiError && err.status === 404,
    ),
    enabled: !!customerId,
  })
  const overview = useQuery({
    queryKey: ['customer-overview', customerId],
    queryFn: () => api.customerOverview(customerId!),
    enabled: !!customerId,
  })
  const settings = useTenantSettings()
  const reads = [subs, plans, profile, overview]
  const currency = settledCustomerCurrency(reads, () => ({
    subscriptions: subs.data?.data,
    plans: plans.data?.data,
    billingProfileCurrency: profile.data?.currency,
    invoices: overview.data?.recent_invoices,
    workspaceDefault: settings.isSuccess ? settings.data.default_currency : undefined,
  }))
  if (currency) return { currency }
  // A read still loading may yet answer. Once none is, a failed one is why
  // there is no answer.
  const failed = [...reads, settings].filter(q => q.isError)
  if (failed.length === 0 || reads.some(q => q.isLoading)) return {}
  return {
    failure: {
      error: failed[0].error,
      retry: () => { for (const q of failed) void q.refetch() },
    },
  }
}
