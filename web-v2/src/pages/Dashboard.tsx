import { useMemo, useState } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { Link } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { api, formatCents } from '@/lib/api'
import { outstandingText } from '@/lib/money'
import type { Invoice, Customer } from '@/lib/api'
import { timeAgo, wallClockNow } from '@/lib/effectiveNow'
import { useEffectiveNowResolver, useTestClocks } from '@/hooks/useClockFrozenMap'
import { useAuth } from '@/contexts/AuthContext'
import { NoticeRow } from '@/components/NoticeRow'
import { SIMULATED_TOTALS_SENTENCE, SIMULATED_TOTALS_TITLE, showSimulatedTotalsNote } from '@/lib/homeNotices'
import { Layout } from '@/components/Layout'
import { LoadError } from '@/components/LoadError'
import { FeedSkeleton } from '@/components/ui/TableSkeleton'
import { SimulatedBadge } from '@/components/TestClockBadge'
import { cn } from '@/lib/utils'
import { Card, CardContent } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { AlertTriangle, FlaskConical, Info } from 'lucide-react'
import {
  Area, AreaChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis,
} from 'recharts'
import { statusBadgeVariant } from '@/lib/status'
import { TrendCard } from '@/components/analytics/TrendCard'
import { TrendCardSkeleton, ChartCardSkeleton } from '@/components/analytics/CardSkeleton'
import { PeriodTabs } from '@/components/analytics/PeriodTabs'
import { PERIOD_LABELS, type Period } from '@/components/analytics/period'
import { tooltipStyle, useChartTheme } from '@/lib/chartTheme'
import { revenueBucketLabel } from '@/lib/chartLabels'

function formatCompactAmount(cents: number): string {
  const abs = Math.abs(cents)
  if (abs >= 100_000_00) return `$${Math.round(cents / 100_00)}K`
  if (abs >= 1_000_00) return `$${(cents / 100_00).toFixed(1)}K`
  return `$${Math.round(cents / 100)}`
}

export default function DashboardPage() {
  usePageTitle('Dashboard')
  const [period, setPeriod] = useState<Period>('30d')
  const theme = useChartTheme()

  const { data: overview, isLoading: overviewLoading, error: errorObj, refetch, dataUpdatedAt } = useQuery({
    queryKey: ['dashboard-overview', period],
    queryFn: () => api.getAnalyticsOverview(period),
  })
  const { data: chartRes, isLoading: chartLoading } = useQuery({
    queryKey: ['dashboard-chart', period],
    queryFn: () => api.getRevenueChart(period),
  })
  const { data: recentInvoices, isLoading: recentInvoicesLoading } = useQuery({
    queryKey: ['dashboard-recent-invoices'],
    queryFn: () => api.listInvoices('limit=5'),
  })

  // Resolve each recent invoice's simulated "now" so the "X ago" label reads
  // against the test clock, not wall-clock (a clock-advanced invoice's created_at
  // is simulated). The pin is customer-level and immutable, and subs inherit it
  // (ADR-027) — so the customer's test_clock_id resolves BOTH subscription and
  // one-off invoices; no need to fetch subscriptions. Only the ≤5 recent
  // invoices' customers are fetched (by id), and clocks come from the shared
  // cached map. Non-pinned customers resolve to undefined → Date.now().
  const recentCustomerIds = useMemo(
    () => [...new Set((recentInvoices?.data ?? []).map(i => i.customer_id).filter(Boolean))],
    [recentInvoices],
  )
  const { data: recentCustomers } = useQuery({
    queryKey: ['dashboard-recent-invoice-customers', recentCustomerIds],
    queryFn: () => api.listCustomers(`ids=${recentCustomerIds.join(',')}&limit=${recentCustomerIds.length}`),
    enabled: recentCustomerIds.length > 0,
  })
  const customerMap = useMemo(() => {
    const m: Record<string, Customer> = {}
    ;(recentCustomers?.data ?? []).forEach(c => { m[c.id] = c })
    return m
  }, [recentCustomers])
  const resolveNow = useEffectiveNowResolver()
  // Totals leave out test-clock customers (analytics/simfilter.go) while
  // Recent Invoices lists their invoices; the note says so where it matters.
  const { user } = useAuth()
  const { data: clocks } = useTestClocks()
  const simulatedNote = showSimulatedTotalsNote(user?.livemode ?? false, clocks?.data?.length ?? 0)

  const chartData = useMemo(() => chartRes?.data ?? [], [chartRes])

  const sparklineRevenue = useMemo(
    () => chartData.map(d => d.revenue_cents).slice(-12),
    [chartData],
  )

  // Attention items surface things the operator should act on. Empty list = quiet state.
  const alerts = useMemo(() => {
    if (!overview) return []
    // Tone follows urgency: failed payments and stuck refunds are money at
    // risk (red); invoices in payment retry need watching (amber, the same
    // colour as the sidebar count); open invoices are normal (neutral).
    const items: { severity: AlertSeverity; text: string; to?: string }[] = []
    if (overview.failed_payments > 0) {
      items.push({
        severity: 'danger',
        text: `${overview.failed_payments} failed ${overview.failed_payments === 1 ? 'payment' : 'payments'} in the last ${PERIOD_LABELS[period].toLowerCase()}`,
        to: '/invoices?payment_status=failed',
      })
    }
    if (overview.dunning_active > 0) {
      items.push({
        severity: 'warning',
        text: `${overview.dunning_active} active dunning ${overview.dunning_active === 1 ? 'run' : 'runs'}`,
        to: '/dunning',
      })
    }
    if (overview.refunds_needing_attention > 0) {
      // A failed/pending refund = money owed to a customer that hasn't been
      // pushed back. The refund is operator-retried (no auto-sweep), so this is
      // the nudge that makes a stuck refund visible. Links to the filtered list.
      items.push({
        severity: 'danger',
        text: `${overview.refunds_needing_attention} ${overview.refunds_needing_attention === 1 ? 'refund needs' : 'refunds need'} attention`,
        to: '/credit-notes?refund_status=needs_attention',
      })
    }
    // The count covers every currency, so the amount names every currency
    // too (outstanding_ar alone is the default currency only: a tenant owed
    // only in EUR lost this row, and a mixed one saw a USD-only sum).
    const outstanding = overview.outstanding_by_currency ?? []
    if (overview.open_invoices > 0 && outstanding.some(a => a.amount_cents > 0)) {
      items.push({
        severity: 'info',
        text: outstandingText(overview.open_invoices, outstanding),
        to: '/invoices?status=finalized',
      })
    }
    return items
  }, [overview, period])

  return (
    <Layout>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold text-foreground">Dashboard</h1>
          {dataUpdatedAt > 0 && (
            <p className="text-xs text-muted-foreground mt-1" aria-live="polite">
              {/* Query-freshness is genuinely wall-clock: it reports when THIS
                  browser last refetched, independent of any test clock. */}
              Updated {timeAgo(new Date(dataUpdatedAt).toISOString(), wallClockNow())}
            </p>
          )}
        </div>
        <div className="flex items-center gap-2">
          <PeriodTabs value={period} onChange={setPeriod} />
        </div>
      </div>

      {errorObj ? (
        <Card className="mt-6">
          <CardContent className="p-0">
            <LoadError what="the dashboard" error={errorObj} onRetry={() => refetch()} />
          </CardContent>
        </Card>
      ) : overviewLoading || !overview ? (
        <div className="grid grid-cols-2 lg:grid-cols-4 gap-4 mt-6">
          {[0, 1, 2, 3].map(i => <TrendCardSkeleton key={i} />)}
        </div>
      ) : (
        <>
          {simulatedNote && (
            <NoticeRow
              icon={FlaskConical}
              title={SIMULATED_TOTALS_TITLE}
              className="mt-4"
              action={
                <Link to="/test-clocks" className="text-sm font-medium text-primary hover:underline">
                  Test clocks
                </Link>
              }
            >
              {SIMULATED_TOTALS_SENTENCE}
            </NoticeRow>
          )}

          {/* Alerts row */}
          {alerts.length > 0 && (
            <div className="mt-6 space-y-2" role="region" aria-label="Attention required">
              {alerts.map((a, i) => (
                <AlertRow key={i} severity={a.severity} text={a.text} to={a.to} />
              ))}
            </div>
          )}

          {/* KPI row */}
          <div className="grid grid-cols-2 lg:grid-cols-4 gap-4 mt-6">
            <TrendCard
              label="MRR"
              value={formatCents(overview.mrr, overview.currency)}
              current={overview.mrr}
              previous={overview.mrr_prev}
              sparkline={sparklineRevenue}
              hint={`ARR ${formatCents(overview.arr, overview.currency)}`}
            />
            <TrendCard
              label="Revenue"
              value={formatCents(overview.revenue, overview.currency)}
              current={overview.revenue}
              previous={overview.revenue_prev}
              hint={`${overview.paid_invoices} ${overview.paid_invoices === 1 ? 'invoice' : 'invoices'} paid`}
            />
            <TrendCard
              label="Active Customers"
              value={overview.active_customers}
              current={overview.active_customers}
              previous={overview.active_customers - overview.new_customers}
              hint={
                <>
                  +{overview.new_customers} new · {overview.active_subscriptions} subs
                  {overview.trialing_subscriptions > 0 && ` · ${overview.trialing_subscriptions} trialing`}
                </>
              }
            />
            <TrendCard
              label="Failed Payments"
              value={overview.failed_payments}
              current={overview.failed_payments}
              previous={0}
              inverseDelta
              valueTone={overview.failed_payments > 0 ? 'danger' : 'default'}
              hint={`${PERIOD_LABELS[period]}`}
              sparklineTone="danger"
            />
          </div>

          {/* Revenue chart */}
          {chartLoading ? (
            <div className="mt-6"><ChartCardSkeleton height={220} /></div>
          ) : (
            <Card className="mt-6">
              <CardContent className="p-5">
                <div className="flex items-center justify-between mb-4">
                  <div>
                    <p className="text-sm font-medium text-foreground">Revenue</p>
                    <p className="text-xs text-muted-foreground mt-0.5">Last {PERIOD_LABELS[period].toLowerCase()}</p>
                  </div>
                </div>
                {chartData.length > 0 ? (
                  <div role="img" aria-label={`Revenue trend over the last ${PERIOD_LABELS[period].toLowerCase()}`}>
                  <ResponsiveContainer width="100%" height={220}>
                    <AreaChart data={chartData} margin={{ top: 5, right: 5, left: 5, bottom: 5 }}>
                      <defs>
                        <linearGradient id="dashRev" x1="0" y1="0" x2="0" y2="1">
                          <stop offset="0%" stopColor={theme.primary} stopOpacity={0.18} />
                          <stop offset="100%" stopColor={theme.primary} stopOpacity={0} />
                        </linearGradient>
                      </defs>
                      <CartesianGrid strokeDasharray="3 3" vertical={false} stroke={theme.grid} />
                      <XAxis
                        dataKey="date"
                        tickFormatter={revenueBucketLabel}
                        tick={{ fontSize: 11, fill: theme.tick }}
                        axisLine={false}
                        tickLine={false}
                      />
                      <YAxis
                        tickFormatter={formatCompactAmount}
                        tick={{ fontSize: 11, fill: theme.tick }}
                        axisLine={false}
                        tickLine={false}
                        width={55}
                      />
                      <Tooltip
                        formatter={(value) => [formatCents(Number(value)), 'Revenue']}
                        labelFormatter={(label) => revenueBucketLabel(String(label))}
                        contentStyle={tooltipStyle(theme)}
                        itemStyle={{ color: theme.tooltipText }}
                        cursor={{ fill: theme.grid }}
                      />
                      <Area
                        type="monotone"
                        dataKey="revenue_cents"
                        stroke={theme.primary}
                        strokeWidth={2}
                        fill="url(#dashRev)"
                        dot={false}
                        activeDot={{ r: 4, fill: theme.tooltipBg, stroke: theme.primary, strokeWidth: 2 }}
                      />
                    </AreaChart>
                  </ResponsiveContainer>
                  </div>
                ) : (
                  <div className="h-[220px] flex items-center justify-center text-sm text-muted-foreground">
                    {/* The chart sums the workspace currency only, like the
                        Revenue card, so the empty text names it. */}
                    No paid {overview.currency?.toUpperCase()} invoices in this period.
                  </div>
                )}
              </CardContent>
            </Card>
          )}

          {/* Recent activity */}
          <Card className="mt-6">
            <CardContent className="p-0">
              <div className="flex items-center justify-between px-5 py-3 border-b border-border">
                <p className="text-sm font-medium text-foreground">Recent Invoices</p>
                <Link to="/invoices" className="text-xs text-muted-foreground hover:text-foreground transition-colors">
                  View all →
                </Link>
              </div>
              {recentInvoicesLoading ? (
                <FeedSkeleton rows={5} />
              ) : recentInvoices?.data && recentInvoices.data.length > 0 ? (
                <div className="divide-y divide-border">
                  {recentInvoices.data.slice(0, 5).map((inv: Invoice) => (
                    <Link
                      key={inv.id}
                      to={`/invoices/${inv.id}`}
                      className="flex items-center justify-between px-5 py-3 hover:bg-muted/50 transition-colors"
                    >
                      <div className="flex items-center gap-3 min-w-0">
                        <PaymentStatusDot status={inv.payment_status} />
                        <span className="text-sm font-mono text-foreground">{inv.invoice_number}</span>
                        {/* Drafts: show "draft" pill instead of payment_status —
                            payment_status is meaningless before finalize. */}
                        {inv.status === 'draft' ? (
                          <Badge variant={statusBadgeVariant('draft')} className="text-[10px]">
                            draft
                          </Badge>
                        ) : (
                          <Badge variant={statusBadgeVariant(inv.payment_status)} className="text-[10px]">
                            {inv.payment_status}
                          </Badge>
                        )}
                        {/* Authoritative per-invoice flag — same rule as the
                            Invoices list; covers manual one-off invoices that
                            the old sub→clock lookup missed. */}
                        {inv.is_simulated && <SimulatedBadge />}
                      </div>
                      <div className="flex items-center gap-4 shrink-0">
                        <span className="text-sm tabular-nums text-foreground">
                          {formatCents(inv.amount_due_cents, inv.currency)}
                        </span>
                        <span className="text-xs text-muted-foreground w-16 text-right">
                          {/* Relative to the invoice's simulated clock when pinned
                              (created_at is stamped in simulation time on a clock-advanced
                              invoice); wall-clock invoices resolve to wall time. */}
                          {timeAgo(
                            inv.created_at,
                            resolveNow(customerMap[inv.customer_id]?.test_clock_id),
                          )}
                        </span>
                      </div>
                    </Link>
                  ))}
                </div>
              ) : (
                <div className="px-5 py-8 text-center text-sm text-muted-foreground">
                  No invoices yet. They appear here when a subscription bills or you create one.
                </div>
              )}
            </CardContent>
          </Card>

        </>
      )}
    </Layout>
  )
}

type AlertSeverity = 'info' | 'warning' | 'danger'

function AlertRow({ severity, text, to }: { severity: AlertSeverity; text: string; to?: string }) {
  const bg =
    severity === 'danger' ? 'bg-destructive/5 border-destructive/20'
    : severity === 'warning' ? 'bg-amber-500/5 border-amber-500/20'
    : 'bg-muted/40 border-border'
  const iconColor =
    severity === 'danger' ? 'text-destructive'
    : severity === 'warning' ? 'text-amber-600 dark:text-amber-400'
    : 'text-muted-foreground'
  const Icon = severity === 'info' ? Info : AlertTriangle
  const content = (
    <div className={cn('flex items-center gap-2 px-4 py-2 rounded-md border text-sm', bg)}>
      <Icon size={14} className={iconColor} aria-hidden />
      <span className="text-foreground flex-1">{text}</span>
      {to && <span className="text-xs text-muted-foreground">Open →</span>}
    </div>
  )
  return to ? <Link to={to}>{content}</Link> : content
}

function PaymentStatusDot({ status }: { status: string }) {
  const color =
    status === 'succeeded' ? 'bg-emerald-500'
    : status === 'failed' ? 'bg-destructive'
    : status === 'processing' ? 'bg-blue-500'
    : 'bg-amber-500'
  return (
    <span className={cn('w-2 h-2 rounded-full shrink-0', color)} aria-label={`Payment ${status}`} title={status} />
  )
}

