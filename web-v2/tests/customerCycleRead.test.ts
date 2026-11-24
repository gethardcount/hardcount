// The customer page's current-cycle usage read. The card is rendered once on
// the server, from a cache that already holds its reads. Server rendering
// sends no request, so a read missing from the cache is one still loading.
import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { MemoryRouter } from 'react-router-dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { test, vi } from 'vitest'
import assert from 'node:assert/strict'
import { getGetTenantSettingsQueryKey } from '../src/lib/gen/queries.gen.ts'
import { setTenantTimezone } from '../src/lib/api.ts'

// The display zone, as AccountSettingsGate sets it once the settings load.
setTenantTimezone('UTC')

vi.mock('@/contexts/AuthContext', () => ({
  useAuth: () => ({
    user: { user_id: 'usr_1', tenant_id: 'ten_1', email: 'op@example.com', livemode: false },
    loading: false,
  }),
}))

const CUSTOMER = {
  id: 'hc_cus_1',
  external_id: 'acme',
  display_name: 'Acme',
  email: 'billing@acme.example',
  created_at: '2031-01-01T00:00:00Z',
}

function text(html: string): string {
  return html.replace(/<[^>]+>/g, ' ').replace(/&#x27;/g, "'").replace(/\s+/g, ' ')
}

test('the cycle read keeps one entry across visits, and a revisit draws it at once', async () => {
  const { CostDashboard } = await import('../src/components/CostDashboard.tsx')
  const client = new QueryClient({ defaultOptions: { queries: { retryOnMount: false } } })
  client.setQueryData(getGetTenantSettingsQueryKey(), { default_currency: 'USD', timezone: 'UTC' })
  const draw = () => text(renderToStaticMarkup(createElement(QueryClientProvider, { client },
    createElement(MemoryRouter, null, createElement(CostDashboard, { customer: CUSTOMER as never })))))
  const cycleReads = () => client.getQueryCache().findAll({ queryKey: ['customer-usage', CUSTOMER.id, 'cycle'] })

  vi.useFakeTimers({ toFake: ['Date'] })
  try {
    vi.setSystemTime(Date.parse('2031-03-01T12:00:00Z'))
    draw()
    assert.equal(cycleReads().length, 1)
    // The read lands, as a fetch would.
    client.setQueryData(cycleReads()[0].queryKey, {
      customer_id: CUSTOMER.id,
      period: { from: '2031-02-20T00:00:00Z', to: '2031-03-20T00:00:00Z', source: 'current_billing_cycle' },
      subscriptions: [], meters: [], warnings: [], buckets: [],
      totals: [{ currency: 'USD', amount_cents: 1234 }],
    })

    // A later visit, a second later by real time.
    vi.setSystemTime(Date.parse('2031-03-01T12:00:01Z'))
    const page = draw()
    assert.equal(cycleReads().length, 1)
    assert.match(page, /Billed usage \$12\.34/)
  } finally {
    vi.useRealTimers()
  }
})
