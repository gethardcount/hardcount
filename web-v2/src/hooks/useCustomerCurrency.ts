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

// useCustomerCurrency is resolveCustomerCurrency fed from the server: the
// customer's subscriptions, every plan (an archived plan can still bill a live
// subscription), the billing profile, the recent invoices and the account
// settings (whose default currency is the last answer). It is undefined until
// all of them load, and stays undefined if any of them fails
// (settledCustomerCurrency says why).
export function useCustomerCurrency(customerId: string | undefined): CustomerCurrency | undefined {
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
  if (!settings.isSuccess) return undefined
  return settledCustomerCurrency([subs, plans, profile, overview], () => ({
    subscriptions: subs.data?.data,
    plans: plans.data?.data,
    billingProfileCurrency: profile.data?.currency,
    invoices: overview.data?.recent_invoices,
    workspaceDefault: settings.data.default_currency,
  }))
}
