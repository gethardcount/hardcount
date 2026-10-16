import { useQuery } from '@tanstack/react-query'
import { api, type CustomerUsage } from '@/lib/api'
import { lastInvoicedPeriod } from '@/lib/usageWindow'
import { clockNow, useClockFrozenMap } from '@/hooks/useClockFrozenMap'

// Shared queries for the customer page's Activity and Margin cards (ADR-121).

// fetchCustomerUsageOrNull reads GET /v1/customers/{id}/usage. 422
// customer_has_no_subscription is the documented "no current cycle" state and
// 404 a missing customer; both come back as null so a card renders its empty
// state instead of an error. Network and auth errors still throw.
export async function fetchCustomerUsageOrNull(
  customerId: string,
  params?: { from?: string; to?: string },
): Promise<CustomerUsage | null> {
  try {
    return await api.customerUsage(customerId, params)
  } catch (err) {
    if (err && typeof err === 'object' && 'status' in err && 'code' in err) {
      const s = (err as { status: number }).status
      const c = (err as { code?: string }).code
      if (s === 404) return null
      if (s === 422 && c === 'customer_has_no_subscription') return null
    }
    throw err
  }
}

// useCurrentCycleUsage is the customer's current-cycle usage: the server's
// default window, the cycle of the subscription whose cycle started last.
// Activity's Cycle tab and Margin's "Current period" use the same key, so the
// page makes one request for both. The key carries the clock's frozen_time so
// an advance refetches; an unpinned customer keys on 'wall'.
export function useCurrentCycleUsage(customerId: string, testClockId?: string | null, enabled = true) {
  const frozen = clockNow(useClockFrozenMap(), testClockId) ?? 'wall'
  return useQuery({
    queryKey: ['customer-usage', customerId, 'cycle', frozen],
    queryFn: () => fetchCustomerUsageOrNull(customerId),
    enabled,
  })
}

// useLastInvoicedPeriod is the billing period of the customer's most recent
// subscription invoice (see lastInvoicedPeriod), or null when there is none.
// It reads the 20 invoices with the latest period start, which is enough to
// find the latest period end.
export function useLastInvoicedPeriod(customerId: string, enabled = true) {
  return useQuery({
    queryKey: ['customer-last-invoiced-period', customerId],
    queryFn: async () => {
      const qs = new URLSearchParams({ customer_id: customerId, sort: 'period', dir: 'desc', limit: '20' })
      const res = await api.listInvoices(qs.toString())
      return lastInvoicedPeriod(res.data)
    },
    enabled,
  })
}

// useLatestUsageAt is the timestamp of the customer's newest usage event, or
// null when it has none. The usage-events list is ordered newest first.
export function useLatestUsageAt(customerId: string, enabled = true) {
  return useQuery({
    queryKey: ['customer-latest-usage', customerId],
    queryFn: async () => {
      const qs = new URLSearchParams({ customer_id: customerId, limit: '1' })
      const res = await api.listUsageEvents(qs.toString())
      return res.data[0]?.timestamp ?? null
    },
    enabled,
  })
}
