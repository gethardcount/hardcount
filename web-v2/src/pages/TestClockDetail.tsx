import { useEffect, useMemo, useState } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'

import { api, ApiError, formatDateTime, getTenantTimezone, type TestClock, type AdvanceSummary } from '@/lib/api'
import { showApiError } from '@/lib/formErrors'
import { cn } from '@/lib/utils'
import { formatInTimeZone, fromZonedTime } from 'date-fns-tz'
import { Layout } from '@/components/Layout'
import { DetailSkeleton } from '@/components/ui/DetailSkeleton'
import { Button } from '@/components/ui/button'
import { LoadError } from '@/components/LoadError'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent } from '@/components/ui/card'
import { Label } from '@/components/ui/label'
import { DateTimePicker } from '@/components/ui/datetime-picker'
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from '@/components/ui/dialog'
import {
  AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent,
  AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import { useAuth } from '@/contexts/AuthContext'
import { ChevronLeft, FastForward, Loader2, Trash2, AlertTriangle, Clock as ClockIcon, Users } from 'lucide-react'
import { EmptyState } from '@/components/EmptyState'

// LastAdvanceCard summarises what the most recent advance produced, so the
// operator sees the outcome inline instead of cross-checking the Invoices,
// Audit, and Dunning pages. Only non-zero counts are shown; zero across the
// board reads as an honest "nothing was due".
function LastAdvanceCard({ summary }: { summary: AdvanceSummary }) {
  const rows = (
    [
      ['Invoices generated', summary.invoices_generated],
      ['Trials activated', summary.trials_activated],
      ['Collections resumed', summary.pauses_resumed],
      ['Spending thresholds crossed', summary.thresholds_fired],
      ['Tax retries', summary.tax_retried],
      ['Auto-charges retried', summary.charges_retried],
      ['Clawbacks issued', summary.clawbacks_issued],
      ['Credit grants expired', summary.credits_expired],
      ['Dunning steps advanced', summary.dunning_advanced],
    ] as Array<[string, number]>
  ).filter(([, n]) => n > 0)

  // advanced_from is the Go zero time on the recover-in-flight path, and equals
  // advanced_to on a retry — in both cases show only the destination.
  const noFrom = !summary.advanced_from || summary.advanced_from.startsWith('0001-01-01')
  const span =
    noFrom || summary.advanced_from === summary.advanced_to
      ? `Advanced to ${formatDateTime(summary.advanced_to)}`
      : `Advanced ${formatDateTime(summary.advanced_from)} → ${formatDateTime(summary.advanced_to)}`

  return (
    // A finished advance is information: neutral card. Amber only when the
    // advance ended with an error, and then only on the line that says so.
    // Card draws its edge with ring-1, so the amber edge is a ring colour.
    <Card className={cn('mt-4', summary.had_errors && 'ring-amber-300 dark:ring-amber-500/40')}>
      <CardContent className="px-6 py-5">
        <div className="flex items-center gap-2">
          <FastForward size={16} className="text-muted-foreground" />
          <p className="text-sm font-semibold text-foreground">Last advance results</p>
        </div>
        <p className="mt-1 text-xs text-muted-foreground">{span}</p>
        {rows.length === 0 ? (
          <p className="mt-3 text-sm text-muted-foreground">
            No billing activity — nothing was due in this period.
          </p>
        ) : (
          <dl className="mt-3 grid grid-cols-2 gap-x-8 gap-y-2 sm:grid-cols-3">
            {rows.map(([label, n]) => (
              // Value right after its label: spread to the cell's edge, a
              // lone row left its number a column-width away.
              <div key={label} className="flex items-baseline gap-2">
                <dt className="text-sm text-muted-foreground">{label}</dt>
                <dd className="text-sm font-semibold tabular-nums">{n}</dd>
              </div>
            ))}
          </dl>
        )}
        {summary.had_errors && (
          <p className="mt-3 flex items-start gap-1.5 text-xs text-amber-700 dark:text-amber-400">
            <AlertTriangle size={13} className="mt-0.5 shrink-0" aria-hidden />
            This advance ended with an error — the counts above reflect only the work that
            completed before it failed.
          </p>
        )}
      </CardContent>
    </Card>
  )
}

function StatusBadge({ status }: { status: TestClock['status'] }) {
  switch (status) {
    case 'ready':
      return <Badge variant="secondary">Ready</Badge>
    case 'advancing':
      return (
        <Badge variant="outline" className="border-blue-500/30 bg-blue-500/10 text-blue-700 dark:text-blue-400">
          <Loader2 size={10} className="animate-spin mr-1" /> Advancing
        </Badge>
      )
    case 'internal_failure':
      return <Badge variant="destructive">Failed</Badge>
  }
}

export default function TestClockDetailPage() {
  const { id } = useParams<{ id: string }>()
  const { user } = useAuth()
  const navigate = useNavigate()
  const queryClient = useQueryClient()

  const [showAdvance, setShowAdvance] = useState(false)
  const [showDelete, setShowDelete] = useState(false)
  const [retryingAdvance, setRetryingAdvance] = useState(false)
  const [deleting, setDeleting] = useState(false)

  // Test-mode-only — same redirect guard as the index page.
  useEffect(() => {
    if (user?.livemode) navigate('/', { replace: true })
  }, [user?.livemode, navigate])

  const clockQ = useQuery({
    queryKey: ['test-clocks', id],
    queryFn: () => api.getTestClock(id!),
    enabled: !!id && !!user && !user.livemode,
    // Poll while the clock is advancing so the UI catches the back-to-ready
    // transition without a manual refresh. Status field drives the cadence.
    refetchInterval: q => (q.state.data?.status === 'advancing' ? 1500 : false),
  })

  const subsQ = useQuery({
    queryKey: ['test-clocks', id, 'subscriptions'],
    queryFn: () => api.listSubscriptionsOnClock(id!),
    enabled: !!id && !!user && !user.livemode,
  })

  // ADR-027: customer-level attach. Detail page mirrors Stripe's
  // "attached customers" surface — operators primarily think in
  // customers (a simulation is a customer's simulated timeline),
  // not subs. Subs remain visible below for drill-down.
  const customersQ = useQuery({
    queryKey: ['test-clocks', id, 'customers'],
    queryFn: () => api.listAttachedCustomers(id!),
    enabled: !!id && !!user && !user.livemode,
  })

  const clock = clockQ.data
  const subs = subsQ.data?.data ?? []
  const attachedCustomers = customersQ.data?.data ?? []

  const handleRetryAdvance = async () => {
    if (!id) return
    setRetryingAdvance(true)
    try {
      await api.retryAdvanceTestClock(id)
      toast.success('Retrying — catchup running in background')
      // Refresh the clock + its subs. The detail query's poll
      // (every 1.5s while status==='advancing', set up below)
      // picks up the worker-driven transition without us doing
      // anything else. Forcing one immediate refetch makes the
      // status badge flip to "Advancing" without waiting for the
      // first poll tick.
      queryClient.invalidateQueries({ queryKey: ['test-clocks', id] })
    } catch (err) {
      showApiError(err, 'Failed to retry')
    } finally {
      setRetryingAdvance(false)
    }
  }

  const handleDelete = async () => {
    if (!id || deleting) return
    setDeleting(true)
    try {
      await api.deleteTestClock(id)
      toast.success('Test clock deleted')
      // Order matters here:
      //  1. removeQueries (not invalidate) for the just-deleted
      //     entity's keys — invalidate would refetch the still-
      //     mounted detail page's queries against a now-gone ID and
      //     surface a 404. removeQueries cancels any in-flight
      //     request and drops the cache entry.
      //  2. navigate — unmounts the detail page so its query
      //     observers unsubscribe.
      //  3. invalidate the LIST so /test-clocks shows the post-
      //     delete state. The list page is the only mounted
      //     subscriber for this key now, so the refetch hits the
      //     right surface.
      queryClient.removeQueries({ queryKey: ['test-clocks', id] })
      navigate('/test-clocks')
      queryClient.invalidateQueries({ queryKey: ['test-clocks'], exact: true })
    } catch (err) {
      showApiError(err, 'Failed to delete')
      setDeleting(false)
    }
  }

  usePageTitle(clock?.name)

  if (clockQ.isLoading) {
    return (
      <Layout>
        <DetailSkeleton to="/test-clocks" parentLabel="Test Clocks" />
      </Layout>
    )
  }
  if (clockQ.error || !clock) {
    // A 404 is "not found", not a load failure: Retry can't bring a deleted
    // clock back, so it gets the Back link instead.
    const notFound = !clockQ.error || (clockQ.error instanceof ApiError && clockQ.error.status === 404)
    return (
      <Layout>
        <Card className="mt-6">
          {!notFound ? (
            <CardContent className="p-0">
              <LoadError what="this test clock" error={clockQ.error} onRetry={() => clockQ.refetch()} />
            </CardContent>
          ) : (
            <CardContent className="p-8 text-center">
              <p className="text-sm text-destructive mb-3">Test clock not found</p>
              <Link to="/test-clocks">
                <Button variant="outline" size="sm">Back to Test Clocks</Button>
              </Link>
            </CardContent>
          )}
        </Card>
      </Layout>
    )
  }

  return (
    <Layout>
      <div>
        <Link to="/test-clocks" className="inline-flex items-center text-xs text-muted-foreground hover:text-foreground transition-colors">
          <ChevronLeft size={14} /> Back to Test Clocks
        </Link>
      </div>

      {/* Wraps the actions under the title at narrow widths (like the other
          detail pages) instead of truncating the clock name to fit them. */}
      <div className="mt-2 flex flex-wrap items-start justify-between gap-x-4 gap-y-3">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
            <h1 className="text-2xl font-semibold text-foreground break-words min-w-0">
              {clock.name || 'Unnamed clock'}
            </h1>
            <StatusBadge status={clock.status} />
          </div>
          <p className="text-xs text-muted-foreground font-mono mt-1 break-all">{clock.id}</p>
        </div>
        <div className="flex items-center gap-2 shrink-0">
          <Button
            size="sm"
            disabled={clock.status !== 'ready'}
            onClick={() => setShowAdvance(true)}
          >
            <FastForward size={14} className="mr-2" />
            Advance
          </Button>
          <Button size="sm" variant="outline" aria-label="Delete test clock" className="text-destructive hover:text-destructive" onClick={() => setShowDelete(true)}>
            <Trash2 size={14} />
          </Button>
        </div>
      </div>

      {clock.status === 'internal_failure' && (
        <Card className="mt-4 ring-destructive/30 bg-destructive/5">
          <CardContent className="px-6 py-4 flex items-start gap-3">
            <AlertTriangle size={16} className="text-destructive shrink-0 mt-0.5" />
            <div className="text-sm flex-1 min-w-0">
              <p className="font-medium text-destructive">Catchup failed during last advance.</p>
              {clock.last_failure_reason && (
                <p className="text-xs text-muted-foreground mt-1.5 font-mono break-all">
                  Reason: {clock.last_failure_reason}
                </p>
              )}
              <p className="text-muted-foreground mt-2">
                Some invoices may have been generated before the failure — review them below.
                Click <span className="font-medium text-foreground">Retry advance</span> to resume from where catchup stopped, or
                delete this clock to start over.
              </p>
              <div className="mt-3 flex items-center gap-2">
                <Button
                  size="sm"
                  variant="outline"
                  onClick={handleRetryAdvance}
                  disabled={retryingAdvance}
                >
                  {retryingAdvance ? <><Loader2 size={14} className="animate-spin mr-2" />Retrying…</> : 'Retry advance'}
                </Button>
              </div>
            </div>
          </CardContent>
        </Card>
      )}

      {/* Current clock time — the whole point of this page. */}
      <Card className="mt-6">
        <CardContent className="px-6 py-5">
          <div className="flex items-center gap-3">
            <ClockIcon size={20} className="text-violet-500" />
            <div>
              <p className="text-xs uppercase tracking-wider text-muted-foreground">Current clock time</p>
              <p className="text-lg font-mono mt-0.5">{formatDateTime(clock.frozen_time)}</p>
            </div>
          </div>
        </CardContent>
      </Card>

      {/* Last advance results — shown once the catchup has finished (status is
          no longer 'advancing') and a summary exists. */}
      {clock.status !== 'advancing' && clock.last_advance_summary && (
        <LastAdvanceCard summary={clock.last_advance_summary} />
      )}

      {/* Attached customers — Stripe-parity primary surface for a
          test clock detail page (ADR-027 Tier 3). Subs are listed
          below for drill-down. */}
      <div className="mt-8">
        <div className="flex items-center justify-between mb-3">
          <h2 className="text-sm font-semibold text-foreground">Attached customers</h2>
          <p className="text-xs text-muted-foreground">{attachedCustomers.length} pinned</p>
        </div>
        {customersQ.isLoading ? (
          <Card><CardContent className="p-8 text-center text-sm text-muted-foreground">Loading…</CardContent></Card>
        ) : attachedCustomers.length === 0 ? (
          <EmptyState
            icon={Users}
            title="No customers attached"
            description="Customers attached to this clock have all their billing — invoices, dunning, trial expirations — run on the clock's simulated time. Attach one from the Customers page when creating a new customer."
            action={{
              label: 'Go to Customers',
              to: '/customers',
            }}
          />
        ) : (
          <div className="space-y-2">
            {attachedCustomers.map(c => (
              <Link key={c.id} to={`/customers/${c.id}`}>
                <Card className="hover:bg-accent/30 transition-colors">
                  <CardContent className="px-6 py-3 flex items-center gap-4">
                    <div className="flex-1 min-w-0">
                      <p className="text-sm font-medium text-foreground truncate">{c.display_name}</p>
                      <p className="text-xs text-muted-foreground mt-0.5 font-mono">{c.id}</p>
                    </div>
                    <Badge variant="outline">{c.status}</Badge>
                  </CardContent>
                </Card>
              </Link>
            ))}
          </div>
        )}
      </div>

      {/* Subscriptions on this clock — drill-down view; same data
          as the customers list but at sub granularity for operators
          tracking specific billing cycles. */}
      <div className="mt-8">
        <div className="flex items-center justify-between mb-3">
          <h2 className="text-sm font-semibold text-foreground">Subscriptions on this clock</h2>
          <p className="text-xs text-muted-foreground">{subs.length} attached</p>
        </div>
        {subsQ.isLoading ? (
          <Card><CardContent className="p-8 text-center text-sm text-muted-foreground">Loading…</CardContent></Card>
        ) : subs.length === 0 ? (
          <Card>
            <CardContent className="p-8 text-center">
              <p className="text-sm text-muted-foreground">No subscriptions yet — create one for an attached customer above.</p>
            </CardContent>
          </Card>
        ) : (
          <div className="space-y-2">
            {subs.map(s => (
              <Link key={s.id} to={`/subscriptions/${s.id}`}>
                <Card className="hover:bg-accent/30 transition-colors">
                  <CardContent className="px-6 py-3 flex items-center gap-4">
                    <div className="flex-1 min-w-0">
                      <div className="flex items-center gap-2">
                        <p className="text-sm font-medium text-foreground truncate">{s.display_name || s.code}</p>
                        <Badge variant="outline">{s.status}</Badge>
                      </div>
                      <p className="text-xs text-muted-foreground mt-0.5 font-mono">{s.id}</p>
                    </div>
                    <div className="text-xs text-muted-foreground shrink-0 text-right">
                      {s.next_billing_at ? <>Next bill: {formatDateTime(s.next_billing_at)}</> : 'No next-bill'}
                    </div>
                  </CardContent>
                </Card>
              </Link>
            ))}
          </div>
        )}
      </div>

      {showAdvance && (
        <AdvanceClockDialog
          clock={clock}
          subs={subs}
          customerCount={attachedCustomers.length}
          onClose={() => setShowAdvance(false)}
        />
      )}

      <AlertDialog open={showDelete} onOpenChange={open => { if (!open) setShowDelete(false) }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete test clock?</AlertDialogTitle>
            <AlertDialogDescription>
              {attachedCustomers.length > 0 || subs.length > 0 ? (
                <>
                  This permanently deletes the clock and everything in its sandbox
                  {attachedCustomers.length > 0 && (
                    <> — {attachedCustomers.length === 1 ? '1 customer' : `${attachedCustomers.length} customers`}
                    {subs.length > 0 && (
                      <> and {subs.length === 1 ? 'their subscription' : `their ${subs.length} subscriptions`}</>
                    )}</>
                  )}
                  {attachedCustomers.length === 0 && subs.length > 0 && (
                    <> — {subs.length === 1 ? '1 subscription' : `${subs.length} subscriptions`}</>
                  )}
                  {' '}— along with every simulated invoice, usage record, and credit created on it.
                  This can't be undone; real data is never touched.
                </>
              ) : (
                <>
                  This permanently deletes the clock. No customers or subscriptions are pinned to it; nothing else is affected.
                </>
              )}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel onClick={() => setShowDelete(false)}>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={handleDelete} disabled={deleting} className="bg-destructive text-destructive-foreground hover:bg-destructive/90">
              {deleting ? 'Deleting…' : 'Delete clock'}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Layout>
  )
}

type Sub = NonNullable<ReturnType<typeof api.listSubscriptionsOnClock> extends Promise<{ data: infer T }> ? T : never>[number]

function AdvanceClockDialog({
  clock,
  subs,
  customerCount,
  onClose,
}: {
  clock: TestClock
  subs: Sub[]
  customerCount: number
  onClose: () => void
}) {
  const queryClient = useQueryClient()
  // Times shown / picked in tenant TZ so the operator-picked "5pm
  // tomorrow" matches the tenant's clock and the displayed clock-time
  // stays internally consistent. ADR-010.
  const tz = getTenantTimezone() || 'UTC'
  // Date + time edited independently — branded DatePicker for the date
  // and a small text input for HH:mm. Defaults to "current frozen + 1h"
  // in tenant TZ so the picker isn't blank and isn't accidentally past.
  const defaultDate = useMemo(() => {
    const d = new Date(clock.frozen_time)
    d.setHours(d.getHours() + 1)
    return formatInTimeZone(d, tz, 'yyyy-MM-dd')
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [clock.frozen_time, tz])
  const defaultTime = useMemo(() => {
    const d = new Date(clock.frozen_time)
    d.setHours(d.getHours() + 1)
    return formatInTimeZone(d, tz, 'HH:mm')
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [clock.frozen_time, tz])
  const [datePart, setDatePart] = useState(defaultDate)
  const [timePart, setTimePart] = useState(defaultTime)
  const [submitting, setSubmitting] = useState(false)

  // +1mo is a CALENDAR month, not a fixed 31 days — a 31-day jump
  // overshoots the monthly billing boundary the preset exists to hit
  // (Jun 15 + 31d = Jul 16, one day past the Jul 15 cycle close).
  // Month-end CLAMPS (Jan 31 → Feb 28), matching the engine's
  // advanceAnchored arithmetic (ADR-055: anchor day clamps to the
  // target month's last day). The raw setMonth overflow this preset
  // used to ship (Jan 31 → Mar 3) matched the PRE-ADR-055 engine and
  // overshot every month-end boundary after it (2026-07-19 truth audit).
  const presets: { label: string; advance: (d: Date) => Date }[] = [
    { label: '+1h', advance: d => new Date(d.getTime() + 60 * 60 * 1000) },
    { label: '+1d', advance: d => new Date(d.getTime() + 24 * 60 * 60 * 1000) },
    {
      label: '+1mo',
      advance: d => {
        const n = new Date(d)
        const day = n.getDate()
        n.setDate(1)
        n.setMonth(n.getMonth() + 1)
        const lastDay = new Date(n.getFullYear(), n.getMonth() + 1, 0).getDate()
        n.setDate(Math.min(day, lastDay))
        return n
      },
    },
  ]
  const applyPreset = (advance: (d: Date) => Date) => {
    const d = advance(new Date(clock.frozen_time))
    setDatePart(formatInTimeZone(d, tz, 'yyyy-MM-dd'))
    setTimePart(formatInTimeZone(d, tz, 'HH:mm'))
  }

  // Compose date + time in tenant TZ, then re-ground to UTC for both
  // the soft warning and the submit. Single source of truth for the
  // target instant.
  const targetIso = useMemo(() => {
    if (!datePart || !/^\d{2}:\d{2}$/.test(timePart)) return null
    return fromZonedTime(`${datePart}T${timePart}:00`, tz).toISOString()
  }, [datePart, timePart, tz])

  // Stripe-parity hard cap: a single advance call moves the clock by at
  // most 1 year (ADR-028 amendment). Server enforces this too — UI
  // surfaces it early so the operator gets immediate feedback rather
  // than a 422 after submit. Year-arithmetic, not 365 days, so leap
  // years cross cleanly.
  const maxAllowed = useMemo(() => {
    const d = new Date(clock.frozen_time)
    d.setFullYear(d.getFullYear() + 1)
    return d
  }, [clock.frozen_time])
  const exceedsMaxWindow = targetIso !== null && new Date(targetIso) > maxAllowed

  // Soft hint — surfaced when the jump is large but still inside the
  // +1y cap. Reminds the operator catchup will close many cycles.
  const overlongWarning = subs.length > 0 && targetIso !== null && !exceedsMaxWindow && (() => {
    const jumpMs = new Date(targetIso).getTime() - new Date(clock.frozen_time).getTime()
    return jumpMs > 31 * 24 * 60 * 60 * 1000
  })()

  const handleSubmit = async () => {
    if (!targetIso) {
      toast.error('Pick a valid date and time')
      return
    }
    if (new Date(targetIso) <= new Date(clock.frozen_time)) {
      toast.error('Target time must be after the current clock time')
      return
    }
    if (exceedsMaxWindow) {
      toast.error('Advance cannot exceed 1 year per call — chunk longer ranges into successive advances')
      return
    }
    setSubmitting(true)
    try {
      await api.advanceTestClock(clock.id, targetIso)
      toast.success('Clock advancing — billing catchup running')
      queryClient.invalidateQueries({ queryKey: ['test-clocks', clock.id] })
      queryClient.invalidateQueries({ queryKey: ['test-clocks', clock.id, 'subscriptions'] })
      queryClient.invalidateQueries({ queryKey: ['invoices'] })
      onClose()
    } catch (err) {
      showApiError(err, 'Failed to advance clock')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog open onOpenChange={open => { if (!open && !submitting) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Advance clock</DialogTitle>
          <DialogDescription>
            {/* Blast radius up front: everyone on the clock moves together,
                and the operator should know how many that is before
                committing (clock-design review 2026-07-21). */}
            Moves time forward for{' '}
            <strong className="text-foreground">
              {customerCount === 1 ? '1 customer' : `all ${customerCount} customers`}
            </strong>{' '}
            ({subs.length === 1 ? '1 subscription' : `${subs.length} subscriptions`}) pinned to this
            clock, running billing catchup for each.
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-4">
          <div>
            <Label className="text-xs text-muted-foreground">Current clock time</Label>
            <p className="text-sm font-mono mt-1">{formatDateTime(clock.frozen_time)}</p>
          </div>

          <div>
            <Label className="mb-2 block">Quick jumps</Label>
            <div className="flex gap-2">
              {presets.map(p => (
                <Button key={p.label} type="button" size="sm" variant="outline" onClick={() => applyPreset(p.advance)}>
                  {p.label}
                </Button>
              ))}
            </div>
          </div>

          <div>
            <Label>Target clock time</Label>
            <div className="mt-1.5">
              <DateTimePicker
                date={datePart}
                time={timePart}
                onDateChange={setDatePart}
                onTimeChange={setTimePart}
                minDate={new Date(clock.frozen_time)}
              />
            </div>
            <p className="text-xs text-muted-foreground mt-1.5">Times in tenant timezone ({tz}). Each advance moves the clock forward by up to 1 year — chunk longer simulations into successive advances.</p>
            {targetIso !== null && new Date(targetIso) <= new Date(clock.frozen_time) && (
              <p className="text-xs text-destructive mt-1">Target time must be after the current clock time.</p>
            )}
            {exceedsMaxWindow && (
              <p className="text-xs text-destructive mt-1">Target exceeds the 1-year per-advance limit. Pick a date on or before <span className="font-mono">{formatDateTime(maxAllowed.toISOString())}</span>, then advance again.</p>
            )}
          </div>

          {/* Fixed-height slot so the warning appearing/disappearing while
              the operator types a target never shifts the footer buttons —
              a moving Advance button ate a click during the FLOW B9 walk
              (clock-design review 2026-07-21). */}
          <div className="min-h-[3.25rem]">
            {overlongWarning && (
              <div className="px-3 py-2 rounded-md bg-amber-500/10 border border-amber-500/30 text-xs text-amber-900 dark:text-amber-200">
                <strong>Heads up:</strong> jumping more than ~1 month closes many billing cycles in a single
                run. Catchup may take a moment.
              </div>
            )}
          </div>
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={submitting}>Cancel</Button>
          <Button
            onClick={handleSubmit}
            disabled={submitting || targetIso === null || new Date(targetIso) <= new Date(clock.frozen_time) || exceedsMaxWindow}
          >
            {submitting ? <><Loader2 size={14} className="animate-spin mr-2" />Advancing…</> : 'Advance clock'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
