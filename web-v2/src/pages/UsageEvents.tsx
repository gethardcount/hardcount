import { useState, useMemo, useCallback, useEffect } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { useQuery } from '@tanstack/react-query'
import { api, formatDateTime } from '@/lib/api'
import { CustomerCombobox } from '@/components/CustomerCombobox'
import { useTenantTimezone } from '@/hooks/useTenantTimezone'
import { RANGES, RANGE_KEYS, DEFAULT_RANGE, EXPORT_MAX_ROWS, exportCapNotice, parseRange, rangeCaption, windowQueryBounds, usageEmptyKind, needsAnyEventCheck } from '@/lib/usageRange'
import type { Customer, Meter, UsageEvent, UsageEventsAggregate, UsageMeterTotal } from '@/lib/api'
import { downloadCSV } from '@/lib/csv'
import { formatQuantity, quantityWithUnit } from '@/lib/usageQuantity'
import { usageNoneYet } from '@/lib/homeNotices'
import { formatYMDInTZ } from '@/lib/dates'
import { wallClockNow } from '@/lib/effectiveNow'
import { showApiError } from '@/lib/formErrors'
import { Layout } from '@/components/Layout'
import { LoadError } from '@/components/LoadError'
import { toast } from 'sonner'
import { TestClockBadge } from '@/components/TestClockBadge'
import { cn } from '@/lib/utils'
import { fillDays, meterModeBadge, meterValueCaption, sparklinePoints } from '@/lib/usageSummary'
import { KpiStrip } from '@/components/page/KpiStrip'
import { Section } from '@/components/page/Section'
import { Link } from 'react-router-dom'

import { Button } from '@/components/ui/button'
import { DatePicker } from '@/components/ui/date-picker'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import {
  Pagination,
  PaginationContent,
  PaginationItem,
  PaginationLink,
  PaginationNext,
  PaginationPrevious,
} from '@/components/ui/pagination'

import { Download, Activity } from 'lucide-react'
import { EmptyState } from '@/components/EmptyState'
import { TableSkeleton } from '@/components/ui/TableSkeleton'
import { useUrlState } from '@/hooks/useUrlState'
const PAGE_SIZE = 25

export default function UsageEventsPage() {
  usePageTitle('Usage events')
  const [urlState, setUrlState] = useUrlState({
    customer: '',
    meter: '',
    range: DEFAULT_RANGE as string,
    from: '',
    to: '',
    dim: '',
    page: '1',
  })
  const { customer: filterCustomer, meter: filterMeter, from: filterFrom, to: filterTo, dim: filterDim } = urlState
  const range = parseRange(urlState.range)

  // The window is memoised on the filters AND the tenant timezone. On a cold
  // load the page renders before /v1/settings arrives, so the first bounds use
  // the 'UTC' fallback; tz changing when settings land recomputes them.
  // Convert picked civil dates to UTC instants of start/end of day in tenant
  // TZ, never browser-local, so operators in different zones filtering
  // "from May 5" get the same rows. ADR-010.
  // Named activeWindow, not window — shadowing the DOM global in a React
  // component is a debugging trap waiting to happen.
  const tz = useTenantTimezone()
  const bounds = useMemo(
    () => windowQueryBounds(range, { from: filterFrom, to: filterTo }, tz),
    [range, filterFrom, filterTo, tz],
  )
  const activeWindow = bounds.window
  // The sparklines end on today in the tenant zone when the window has no
  // end date (fillDays). Like the bounds, it is recomputed when tz changes.
  const today = useMemo(() => formatYMDInTZ(new Date(wallClockNow()).toISOString(), tz), [tz])
  const { from: fromInstant, to: toInstant } = bounds
  const page = Math.max(1, parseInt(urlState.page) || 1)
  const [events, setEvents] = useState<UsageEvent[]>([])
  const [total, setTotal] = useState(0)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<unknown>(null)
  const [exporting, setExporting] = useState(false)

  // Fetch only the customers referenced by visible events (avoids the
  // "Unknown" pagination bug — see Invoices.tsx for full rationale).
  // Re-fetches when the visible event page changes.
  const customerIdsForEvents = useMemo(() => {
    const set = new Set<string>()
    events.forEach(ev => { if (ev.customer_id) set.add(ev.customer_id) })
    return Array.from(set)
  }, [events])
  const { data: customersData } = useQuery({
    queryKey: ['customers-by-ids-for-usage', customerIdsForEvents],
    queryFn: () => api.listCustomers(customerIdsForEvents.length > 0 ? `ids=${customerIdsForEvents.join(',')}&limit=${customerIdsForEvents.length}` : ''),
    enabled: customerIdsForEvents.length > 0,
  })

  // Full customer list for the FILTER dropdown. The per-page query above
  // only knows customers present on the loaded page — pre-fix the dropdown
  // was populated from it, so on first load (or after a narrowing filter)
  // it offered no options at all. Same pattern as Subscriptions/Invoices.
  // (P11) The filter dropdown is a server-searched CustomerCombobox now —
  // the old bare 50-row list made events from customers past the 50th
  // unfilterable.

  const { data: metersData } = useQuery({
    queryKey: ['meters-ref'],
    queryFn: () => api.listMeters(),
  })

  const customers = customersData?.data ?? []
  const meters = metersData?.data ?? []

  const customerMap = useMemo(() => {
    const cMap: Record<string, Customer> = {}
    customers.forEach(c => { cMap[c.id] = c })
    return cMap
  }, [customers])

  const meterMap = useMemo(() => {
    const mMap: Record<string, Meter> = {}
    meters.forEach(m => { mMap[m.id] = m })
    return mMap
  }, [meters])

  // Filter qs shared between the paginated list fetch and the aggregate
  // endpoint — building it once ensures the stat cards + breakdown can
  // never disagree with the events table on what's "in scope".
  const filterQs = useMemo(() => {
    const parts: string[] = []
    if (filterCustomer) parts.push(`customer_id=${filterCustomer}`)
    if (filterMeter) parts.push(`meter_id=${filterMeter}`)
    if (fromInstant) parts.push(`from=${fromInstant}`)
    if (toInstant) parts.push(`to=${toInstant}`)
    if (filterDim) parts.push(`dimensions=${encodeURIComponent(filterDim)}`)
    return parts.join('&')
  }, [filterCustomer, filterMeter, fromInstant, toInstant, filterDim])

  // Server-side paginated fetch
  const loadEvents = useCallback(() => {
    setLoading(true)
    setError(null)
    const parts: string[] = [
      `limit=${PAGE_SIZE}`,
      `offset=${(page - 1) * PAGE_SIZE}`,
    ]
    if (filterQs) parts.push(filterQs)
    const params = parts.join('&')
    api.listUsageEvents(params)
      .then(res => { setEvents(res.data || []); setTotal(res.total || 0); setLoading(false) })
      .catch(err => { setError(err ?? new Error('Failed to load usage events')); setEvents([]); setTotal(0); setLoading(false) })
  }, [page, filterQs])

  useEffect(() => { loadEvents() }, [loadEvents])

  // Server-side aggregate that powers the stat cards + "Usage by Meter"
  // breakdown. Same filter scope as the events table but no limit/offset
  // — page navigation must NOT shift the totals (issue #7). Decimal
  // precision is preserved end-to-end via string-encoded NUMERIC(38,12).
  // tz (the tenant timezone, above) buckets each meter's daily sparkline by
  // the tenant's calendar day, the same days the date filters use.
  const { data: aggregate } = useQuery<UsageEventsAggregate>({
    queryKey: ['usage-events-aggregate', filterQs, tz],
    queryFn: () => api.aggregateUsageEvents([filterQs, `tz=${encodeURIComponent(tz)}`].filter(Boolean).join('&')),
  })

  // Empty page under a time window: does the workspace have any event at
  // all? Decides "No usage events yet" (set up a meter) against "No events
  // match these filters". Sent only once the windowed list came back empty,
  // with no filters, so a busy workspace never pays for it; the list's
  // total makes this one request count the workspace's events, which on
  // this path are all older than the window.
  const filtersForEmpty = { customer: filterCustomer, meter: filterMeter, dim: filterDim }
  const checkAnyEver = !loading && !error && events.length === 0 && needsAnyEventCheck(range, filtersForEmpty)
  const { data: anyEver = null, isPending: anyEverPending } = useQuery({
    queryKey: ['usage-events-any-ever'],
    queryFn: async () => ((await api.listUsageEvents('limit=1')).data ?? []).length > 0,
    enabled: checkAnyEver,
    retry: false,
  })
  const emptyKind = usageEmptyKind(range, filtersForEmpty, anyEver)
  const noneYet = usageNoneYet(metersData ? meters.length : null)

  // `quantity` is wire-encoded as a string (NUMERIC(38, 12)) for decimal
  // precision; coerce to number for display math only. Authoritative
  // money math stays server-side.
  const eventQuantity = (e: UsageEvent): number => {
    const n = Number(e.quantity)
    return Number.isNaN(n) ? 0 : n
  }

  // Show the dimensions column + filter only when at least one event in the
  // current page actually carries dimensions, OR when a dimension filter is
  // set. Pre-multi-dim tenants stay visually clean.
  const hasDimensions = useMemo(
    () => events.some(e => e.dimensions && Object.keys(e.dimensions).length > 0) || !!filterDim,
    [events, filterDim],
  )

  // One row per meter, in its own unit and under its own aggregation (the
  // server computes `value`). Sorted by name: values in different units
  // have no common order.
  const meterRows = useMemo(() => {
    const rows = aggregate?.by_meter ?? []
    return rows
      .map(r => ({ ...r, meter: meterMap[r.meter_id] as Meter | undefined }))
      .sort((a, b) => (a.meter?.name ?? a.meter_id).localeCompare(b.meter?.name ?? b.meter_id))
  }, [aggregate, meterMap])

  const totalPages = Math.ceil(total / PAGE_SIZE)

  // Export pages through ALL filtered events (capped), not just the server's
  // default page — pre-fix this called listUsageEvents with no limit and silently
  // truncated the CSV to one page.
  //
  // This does NOT mirror AuditLog.tsx any more: that export became a SERVER-side
  // stream with no cap (GET /v1/exports/audit-log.csv, ADR-090 §7) precisely
  // because a browser page-walk with a row cap hands the operator a file that
  // looks complete and is not. This page still page-walks, and still caps at
  // EXPORT_MAX_ROWS — the same latent truncation, on a smaller artifact. Use
  // /v1/exports/usage-events.csv (server-side, uncapped, snapshot-consistent)
  // when that matters; wiring this button to it is the obvious follow-up.
  // When the cap is hit the operator is told (exportCapNotice), using the
  // filtered total the first page reports.
  const EXPORT_PAGE_SIZE = 1000
  const handleExport = async () => {
    setExporting(true)
    try {
      const all: UsageEvent[] = []
      let offset = 0
      let matching = 0
      while (all.length < EXPORT_MAX_ROWS) {
        const parts = [`limit=${EXPORT_PAGE_SIZE}`, `offset=${offset}`]
        if (filterQs) parts.push(filterQs)
        const res = await api.listUsageEvents(parts.join('&'))
        const batch = res.data || []
        if (offset === 0) matching = res.total || 0
        all.push(...batch)
        if (batch.length < EXPORT_PAGE_SIZE) break
        offset += EXPORT_PAGE_SIZE
      }
      // Resolve names for exactly the exported rows via ids= (the old
      // full-list fetch was itself capped at 50 — exported rows beyond
      // it fell back to raw IDs anyway).
      const exportIds = Array.from(new Set(all.map(ev => ev.customer_id).filter(Boolean)))
      const nameById: Record<string, string> = {}
      if (exportIds.length > 0) {
        const res = await api.listCustomers(`ids=${exportIds.join(',')}&limit=${exportIds.length}`)
        res.data.forEach(c => { nameById[c.id] = c.display_name })
      }
      const rows = all.slice(0, EXPORT_MAX_ROWS).map(ev => [
        formatDateTime(ev.timestamp),
        nameById[ev.customer_id] || customerMap[ev.customer_id]?.display_name || ev.customer_id,
        meterMap[ev.meter_id]?.name || ev.meter_id,
        ev.quantity,
        ev.dimensions && Object.keys(ev.dimensions).length > 0 ? JSON.stringify(ev.dimensions) : '',
      ])
      downloadCSV('usage-events.csv', ['Timestamp', 'Customer', 'Meter', 'Value', 'Dimensions'], rows)
      const capped = exportCapNotice(matching)
      if (capped) toast.warning(capped.title, { description: capped.description })
    } catch (err) {
      showApiError(err, 'Export failed')
    } finally {
      setExporting(false)
    }
  }

  // Headline counts come from the server-side aggregate so they reflect
  // ALL filtered rows, not the current page (issue #7). There is no
  // cross-meter unit total: tokens, calls and GB don't add up.
  const totalEvents = aggregate?.total_events ?? total
  const activeMeters = aggregate?.active_meters ?? 0
  const activeCustomers = aggregate?.active_customers ?? 0

  const scopeCaption = rangeCaption(range, activeWindow)

  return (
    <Layout>
      {/* Header */}
      <div className="flex justify-between items-center">
        <div>
          <h1 className="text-2xl font-semibold text-foreground">Usage Events</h1>
          <p className="text-sm text-muted-foreground mt-1">Track and analyze usage across customers and meters</p>
        </div>
        {total > 0 && (
          <Button variant="outline" size="sm" onClick={handleExport} disabled={exporting}>
            <Download size={16} className="mr-2" />
            {exporting ? 'Exporting…' : 'Export CSV'}
          </Button>
        )}
      </div>

      {/* Filter bar */}
      {/* Wraps: 'custom' adds two date pickers, and six controls overflow a
          1440px viewport, clipping the dimension field. */}
      <div className="flex flex-wrap items-center gap-3 mt-6">
        <div className="w-52">
          <CustomerCombobox
            value={filterCustomer}
            onChange={(v) => setUrlState({ customer: v, page: '1' })}
            placeholder="All customers"
            clearable
          />
        </div>
        <select
          value={filterMeter}
          onChange={(e) => setUrlState({ meter: e.target.value, page: '1' })}
          className="flex h-9 w-52 rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-xs transition-colors focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
        >
          <option value="">All meters</option>
          {meters.map(m => (
            <option key={m.id} value={m.id}>{m.name}</option>
          ))}
        </select>
        <select
          value={range}
          onChange={(e) => setUrlState({ range: e.target.value, page: '1' })}
          className="flex h-9 w-40 rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-xs transition-colors focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
        >
          {RANGE_KEYS.map(k => (
            <option key={k} value={k}>{RANGES[k].label}</option>
          ))}
        </select>
        {/* The pickers appear only for 'custom'. Showing them beside an active
            preset would give two controls for one question and no way to tell
            which is in force. */}
        {range === 'custom' && (
          <>
            <DatePicker
              value={filterFrom}
              onChange={v => setUrlState({ from: v, page: '1' })}
              placeholder="From date"
              className="w-44"
            />
            <DatePicker
              value={filterTo}
              onChange={v => setUrlState({ to: v, page: '1' })}
              placeholder="To date"
              className="w-44"
              minDate={filterFrom ? new Date(filterFrom + 'T00:00:00') : undefined}
            />
          </>
        )}
        <input
          type="text"
          value={filterDim}
          onChange={(e) => setUrlState({ dim: e.target.value, page: '1' })}
          placeholder="dimension (e.g. model=gpt-4)"
          className="flex h-9 w-56 rounded-md border border-input bg-transparent px-3 py-1 text-sm font-mono shadow-xs transition-colors focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring placeholder:text-muted-foreground placeholder:font-sans"
        />
      </div>

      {error ? (
        <Card className="mt-6">
          <CardContent className="p-0">
            <LoadError what="usage events" error={error} onRetry={loadEvents} />
          </CardContent>
        </Card>
      ) : loading || (checkAnyEver && anyEverPending) ? (
        <Card className="mt-6">
          <CardContent className="p-0">
            <TableSkeleton columns={4} />
          </CardContent>
        </Card>
      ) : events.length === 0 ? (
        <Card className="mt-6">
          <CardContent className="p-0">
            {emptyKind === 'none-yet' ? (
              <EmptyState
                icon={Activity}
                title="No usage events yet"
                description={noneYet.description}
                action={noneYet.setUpMeter ? { label: 'Set up a meter', to: '/pricing?tab=meters' } : undefined}
              />
            ) : (
              <EmptyState
                icon={Activity}
                title="No events match these filters"
                description="Try a longer date range or clear the filters."
                action={{
                  label: 'Clear filters',
                  variant: 'outline',
                  onClick: () => setUrlState({ customer: '', meter: '', dim: '', from: '', to: '', range: 'all', page: '1' }),
                }}
              />
            )}
          </CardContent>
        </Card>
      ) : (
        <>
          {/* The cards are totals over the SELECTED window, not all history.
              Saying so removes the "where did my events go?" reading when an
              operator lands on the 30-day default. */}
          <p className="text-xs text-muted-foreground mt-6">{scopeCaption}</p>

          <KpiStrip
            className="mt-2"
            items={[
              { label: 'Events', value: totalEvents.toLocaleString() },
              { label: 'Meters with usage', value: activeMeters.toLocaleString() },
              { label: 'Customers with usage', value: activeCustomers.toLocaleString() },
            ]}
          />

          {meterRows.length > 0 && (
            <Section
              className="mt-6"
              title="Usage by meter"
              description="Each meter in its own unit and aggregation. Maximum and Latest value take one reading per customer in this window; billing takes one per billing period."
              flush
            >
              <ul className="divide-y divide-border">
                {meterRows.map(r => (
                  <MeterSummaryRow key={r.meter_id} row={r} meter={r.meter} from={activeWindow.from} to={activeWindow.to} today={today} />
                ))}
              </ul>
            </Section>
          )}

          {/* Events table */}
          <Card className="mt-6">
            <CardHeader className="pb-3">
              <CardTitle className="text-sm">Event Log</CardTitle>
            </CardHeader>
            <CardContent className="p-0">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead className="text-xs font-medium">Timestamp</TableHead>
                    <TableHead className="text-xs font-medium">Customer</TableHead>
                    <TableHead className="text-xs font-medium">Meter</TableHead>
                    {hasDimensions && <TableHead className="text-xs font-medium">Dimensions</TableHead>}
                    <TableHead className="text-xs font-medium text-right">Value</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {events.map(ev => {
                    const v = eventQuantity(ev)
                    // Formatted as text, not through a float, so every digit
                    // the server stored is kept (only separators are added).
                    const display = formatQuantity(ev.quantity)
                    const dims = ev.dimensions && Object.keys(ev.dimensions).length > 0 ? ev.dimensions : null
                    return (
                      <TableRow key={ev.id}>
                        <TableCell className="text-sm text-foreground">
                          <span className="inline-flex items-center gap-2">
                            {formatDateTime(ev.timestamp)}
                            {/* Events for clock-pinned customers are stamped at the
                                clock's simulated instant, not wall-clock ingestion
                                time (clock-design review 2026-07-21). */}
                            {customerMap[ev.customer_id]?.test_clock_id && (
                              <TestClockBadge testClockId={customerMap[ev.customer_id].test_clock_id!} />
                            )}
                          </span>
                        </TableCell>
                        <TableCell className="text-sm text-muted-foreground">
                          {customerMap[ev.customer_id]?.display_name || ev.customer_id.slice(0, 8) + '...'}
                        </TableCell>
                        <TableCell className="text-sm text-muted-foreground">
                          {meterMap[ev.meter_id]?.name || ev.meter_id.slice(0, 8) + '...'}
                        </TableCell>
                        {hasDimensions && (
                          <TableCell>
                            {dims ? (
                              <div className="flex flex-wrap gap-1">
                                {Object.entries(dims).map(([k, val]) => (
                                  <span key={k} className="inline-flex items-center text-xs font-mono bg-muted px-1.5 py-0.5 rounded">
                                    {k}=<span className="text-foreground">{String(val)}</span>
                                  </span>
                                ))}
                              </div>
                            ) : (
                              <span className="text-xs text-muted-foreground">—</span>
                            )}
                          </TableCell>
                        )}
                        <TableCell className={cn('text-sm font-medium text-right tabular-nums', v < 0 ? 'text-destructive' : 'text-foreground')}>
                          {v < 0 ? '' : '+'}{display}
                        </TableCell>
                      </TableRow>
                    )
                  })}
                </TableBody>
              </Table>

              {/* Pagination */}
              {totalPages > 1 && (
                <div className="border-t border-border px-4 py-3 flex items-center justify-between">
                  <p className="text-xs text-muted-foreground">
                    Showing {(page - 1) * PAGE_SIZE + 1}
                    {'\u2013'}
                    {Math.min(page * PAGE_SIZE, total)} of {total}
                  </p>
                  <Pagination>
                    <PaginationContent>
                      <PaginationItem>
                        <PaginationPrevious
                          onClick={() => setUrlState({ page: String(Math.max(1, page - 1)) })}
                          className={cn(page <= 1 && 'pointer-events-none opacity-50')}
                        />
                      </PaginationItem>
                      {Array.from({ length: Math.min(totalPages, 5) }, (_, i) => {
                        let pageNum: number
                        if (totalPages <= 5) {
                          pageNum = i + 1
                        } else if (page <= 3) {
                          pageNum = i + 1
                        } else if (page >= totalPages - 2) {
                          pageNum = totalPages - 4 + i
                        } else {
                          pageNum = page - 2 + i
                        }
                        return (
                          <PaginationItem key={pageNum}>
                            <PaginationLink
                              onClick={() => setUrlState({ page: String(pageNum) })}
                              isActive={page === pageNum}
                            >
                              {pageNum}
                            </PaginationLink>
                          </PaginationItem>
                        )
                      })}
                      <PaginationItem>
                        <PaginationNext
                          onClick={() => setUrlState({ page: String(Math.min(totalPages, page + 1)) })}
                          className={cn(page >= totalPages && 'pointer-events-none opacity-50')}
                        />
                      </PaginationItem>
                    </PaginationContent>
                  </Pagination>
                </div>
              )}
            </CardContent>
          </Card>
        </>
      )}
    </Layout>
  )
}

// MeterSummaryRow is one meter on the Usage page: name and aggregation,
// the value in the meter's unit with a caption saying what it is, and a
// daily sparkline over the selected window.
function MeterSummaryRow({ row, meter, from, to, today }: { row: UsageMeterTotal; meter?: Meter; from?: string; to?: string; today: string }) {
  const name = meter?.name ?? row.meter_id
  const unit = row.aggregation === 'count' ? 'event' : (meter?.unit ?? '')
  const values = fillDays(row.daily ?? [], from || undefined, to || undefined, today)
  const W = 160
  const H = 32
  // A single day has no shape to draw.
  const points = values.length > 1 ? sparklinePoints(values, W, H) : ''
  return (
    <li className="flex flex-col gap-3 px-5 py-4 sm:flex-row sm:items-center">
      <div className="min-w-0 sm:w-56 sm:shrink-0">
        {/* The full name wraps; a cut-off name could not be read anywhere. */}
        <div className="flex items-start gap-2">
          {meter ? (
            <Link to={`/meters/${meter.id}`} className="min-w-0 break-words text-sm font-medium text-foreground hover:underline">{name}</Link>
          ) : (
            <span className="min-w-0 break-words text-sm font-medium text-foreground">{name}</span>
          )}
          <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-2xs text-muted-foreground">{meterModeBadge(row.aggregation)}</span>
        </div>
        <p className="mt-0.5 text-xs text-muted-foreground">
          {row.events.toLocaleString()} {row.events === 1 ? 'event' : 'events'} · {row.customers.toLocaleString()} {row.customers === 1 ? 'customer' : 'customers'}
        </p>
      </div>
      <div className="min-w-0 flex-1">
        <p className="text-lg font-semibold tabular-nums text-foreground">{quantityWithUnit(row.value, unit)}</p>
        <p className="text-xs text-muted-foreground">{meterValueCaption(row.aggregation, row.customers)}</p>
      </div>
      {points && (
        <svg
          width={W}
          height={H}
          viewBox={`-1 -2 ${W + 2} ${H + 4}`}
          className="shrink-0 text-primary"
          role="img"
          aria-label={`${name}: daily usage, ${values.length} days`}
        >
          <polyline points={points} fill="none" stroke="currentColor" strokeWidth={1.5} strokeLinejoin="round" strokeLinecap="round" />
        </svg>
      )}
    </li>
  )
}
