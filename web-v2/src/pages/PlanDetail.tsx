import { useState, useMemo } from 'react'
import { StaleNotice } from '@/components/StaleNotice'
import { usePageTitle } from '@/hooks/usePageTitle'
import { useParams, Link, useNavigate } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'
import { toast } from 'sonner'
import { api, formatCents, formatDate, formatDateTime, type PlanDetail, type Customer } from '@/lib/api'
import { applyApiError } from '@/lib/formErrors'
import { Layout } from '@/components/Layout'
import { LoadError } from '@/components/LoadError'
import { Callout } from '@/components/Callout'
import { TestClockBadge } from '@/components/TestClockBadge'
import { useEffectiveNowResolver } from '@/hooks/useClockFrozenMap'
import { collectionPausedFlag, genericStatus, subscriptionStatus } from '@/lib/statusLabels'
import { StatusBadge } from '@/components/StatusBadge'

import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle,
} from '@/components/ui/dialog'
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from '@/components/ui/table'
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from '@/components/ui/select'

import { Loader2, Lock, Pencil } from 'lucide-react'
import { NoticeRow } from '@/components/NoticeRow'
import { PLAN_LOCKED_TITLE, planLockedSentence } from '@/lib/pricingCopy'
import { editPlanBaseFee } from '@/lib/planBaseFee'
import { PlanPricesCard } from '@/components/PlanPricesCard'
import { CopyButton } from '@/components/CopyButton'
import { DetailBreadcrumb } from '@/components/DetailBreadcrumb'
import { DetailSkeleton } from '@/components/ui/DetailSkeleton'


// Static option sets — single source of truth for both the dropdown
// <SelectItem>s and the Base UI `items` prop that lets <SelectValue>
// render the selected option's label (Base UI shows the raw value
// otherwise).
const BILL_TIMING_OPTIONS = [
  { value: 'in_arrears', label: 'At end of period' },
  { value: 'in_advance', label: 'At start of period' },
]
const PLAN_STATUS_OPTIONS = [
  { value: 'active', label: 'Active' },
  { value: 'archived', label: 'Archived' },
]

const editPlanSchema = z.object({
  name: z.string().min(1, 'Name is required'),
})
type EditPlanData = z.infer<typeof editPlanSchema>

export default function PlanDetailPage() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  // Each subscription's "now" (its test clock when pinned), for when a timed
  // collection pause ends.
  const resolveNow = useEffectiveNowResolver()
  const [showEdit, setShowEdit] = useState(false)

  const { data: plan, isLoading, error: loadError, refetch } = useQuery({
    queryKey: ['plan', id],
    queryFn: () => api.getPlan(id!),
    enabled: !!id,
  })

  const { data: meters } = useQuery({
    queryKey: ['meters'],
    queryFn: () => api.listMeters(),
    select: (res) => res.data,
  })

  const { data: subsPage } = useQuery({
    queryKey: ['plan-subscriptions', id],
    // Filtered by the server, for the same reason as the customer page. The
    // server returns at most 100; `total` is the real count.
    queryFn: () => api.listSubscriptions(`plan_id=${encodeURIComponent(id!)}&limit=100`),
    enabled: !!id,
  })
  const subsData = subsPage?.data

  // ids= of the subscribers actually shown — the old bare 50-row page
  // rendered raw customer ids for anyone past the 50th (P11).
  const planSubCustomerIds = useMemo(
    () => Array.from(new Set((Array.isArray(subsData) ? subsData : []).map(s => s.customer_id).filter(Boolean))),
    [subsData],
  )
  const { data: customersData } = useQuery({
    queryKey: ['customers-map', planSubCustomerIds],
    queryFn: () =>
      api.listCustomers(
        planSubCustomerIds.length > 0
          ? `ids=${planSubCustomerIds.join(',')}&limit=${planSubCustomerIds.length}`
          : '',
      ),
    enabled: planSubCustomerIds.length > 0,
  })
  const customerMap = useMemo(() => {
    const map: Record<string, Customer> = {}
    ;(customersData?.data ?? []).forEach(c => { map[c.id] = c })
    return map
  }, [customersData])

  const subscriptions = Array.isArray(subsData) ? subsData : []
  const subsTotal = subsPage?.total ?? subscriptions.length
  const allMeters = Array.isArray(meters) ? meters : []

  const activeSubscriptions = useMemo(() =>
    subscriptions.filter(s => s.status === 'active'),
  [subscriptions])

  // The server's count of subscriptions that keep the base fee fixed (not
  // this page's own 'active' filter, which misses trialing, paused and draft
  // subscriptions the lock counts).
  const liveSubscriptions = plan?.live_subscription_count ?? 0
  const locked = liveSubscriptions > 0

  const loading = isLoading

  usePageTitle(plan?.name)

  if (loading) {
    return (
      <Layout>
        <DetailSkeleton to="/pricing" parentLabel="Plans" />
      </Layout>
    )
  }

  // A failed refresh after a load keeps the page (StaleNotice below);
  // only a page that never loaded becomes the error screen.
  if (loadError && !plan) {
    return (
      <Layout>
        <LoadError what="this plan" error={loadError} onRetry={() => refetch()} className="py-16" />
      </Layout>
    )
  }

  if (!plan) {
    return (
      <Layout>
        <p className="text-sm text-muted-foreground py-16 text-center">Plan not found</p>
      </Layout>
    )
  }

  return (
    <Layout>
      <DetailBreadcrumb to="/pricing" parentLabel="Plans" currentLabel={plan.name} />
      {loadError && <StaleNotice what="this plan" onRetry={() => refetch()} />}

      {/* Header */}
      <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-3 mb-6">
        <div className="min-w-0">
          <h1 className="text-2xl font-semibold text-foreground">{plan.name}</h1>
          <div className="flex flex-wrap items-center gap-2 mt-1">
            <span className="text-xs text-muted-foreground font-mono bg-muted px-2 py-0.5 rounded">{plan.id}</span>
            <CopyButton text={plan.id} />
            <span className="text-xs text-muted-foreground font-mono bg-muted px-2 py-0.5 rounded">{plan.code}</span>
            <span className="text-xs text-muted-foreground">Created {formatDateTime(plan.created_at)}</span>
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-3">
          <StatusBadge status={genericStatus(plan.status)} />
          <Button variant="outline" size="sm" onClick={() => setShowEdit(true)}>
            <Pencil size={14} className="mr-1.5" />
            Edit
          </Button>
        </div>
      </div>

      {/* Why the base fee is fixed: one line, not a box. The subscriptions
          it counts are listed further down. */}
      {locked && (
        <NoticeRow
          icon={Lock}
          title={PLAN_LOCKED_TITLE}
          className="mb-4"
          action={
            <Link to="/pricing?tab=plans" className="text-sm font-medium text-primary hover:underline">
              Go to plans
            </Link>
          }
        >
          {planLockedSentence(liveSubscriptions)}
        </NoticeRow>
      )}

      {/* Key metrics. The plan's settings once each: the Properties card
          below them repeated interval, currency, status, code and ID, and
          pushed the prices down (L-08). When the base fee is billed is part
          of the price, so it sits under it. */}
      <Card>
        <CardContent className="p-0">
          <div className="grid grid-cols-2 divide-border md:flex md:divide-x">
            <div className="min-w-0 px-6 py-4 md:flex-1">
              <p className="text-sm text-muted-foreground">Base price</p>
              <p className="text-lg font-semibold text-foreground mt-1">{formatCents(plan.base_amount_cents, plan.currency)}</p>
              <p className="text-xs text-muted-foreground mt-0.5">
                {plan.base_bill_timing === 'in_advance' ? 'Billed at the start of each period' : 'Billed at the end of each period'}
              </p>
            </div>
            <div className="min-w-0 px-6 py-4 md:flex-1">
              <p className="text-sm text-muted-foreground">Interval</p>
              <p className="text-lg font-semibold text-foreground mt-1">{plan.billing_interval === 'yearly' ? 'Yearly' : 'Monthly'}</p>
            </div>
            <div className="min-w-0 px-6 py-4 md:flex-1">
              <p className="text-sm text-muted-foreground">Currency</p>
              <p className="text-lg font-semibold text-foreground mt-1">{plan.currency.toUpperCase()}</p>
            </div>
            <div className="min-w-0 px-6 py-4 md:flex-1">
              <p className="text-sm text-muted-foreground">Active subscriptions</p>
              <p className="text-lg font-semibold text-foreground mt-1">{activeSubscriptions.length}</p>
            </div>
          </div>
        </CardContent>
      </Card>

      <PlanPricesCard plan={plan} meters={allMeters} />

      {/* Subscriptions */}
      <Card className="mt-6">
        <CardHeader>
          <div className="flex items-center justify-between">
            {/* No count until the list arrives: "(0)" read as "none" (W-17). */}
            <CardTitle className="text-sm">Subscriptions{subsPage ? ` (${subsTotal})` : ''}</CardTitle>
            {subscriptions.length > 0 && (
              <Link to="/subscriptions" className="text-sm text-muted-foreground hover:text-foreground transition-colors">
                View all
              </Link>
            )}
          </div>
        </CardHeader>
        <CardContent className="p-0">
          {subscriptions.length > 0 ? (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead>
                  <TableHead>Customer</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Current period</TableHead>
                  <TableHead>Created</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {subscriptions.map(sub => (
                  <TableRow
                    key={sub.id}
                    className="cursor-pointer hover:bg-muted/50 transition-colors"
                    onClick={(e) => {
                      const target = e.target as HTMLElement
                      if (target.closest('button, a, input, select')) return
                      navigate(`/subscriptions/${sub.id}`)
                    }}
                  >
                    <TableCell>
                      <span className="inline-flex items-center gap-2">
                        <Link to={`/subscriptions/${sub.id}`} className="text-sm font-medium text-foreground hover:text-primary transition-colors">
                          {sub.display_name}
                        </Link>
                        {sub.test_clock_id && <TestClockBadge testClockId={sub.test_clock_id} />}
                      </span>
                      <p className="text-xs text-muted-foreground font-mono">{sub.code}</p>
                    </TableCell>
                    <TableCell>
                      <Link to={`/customers/${sub.customer_id}`} className="text-sm text-primary hover:underline">
                        {customerMap?.[sub.customer_id]?.display_name || sub.customer_id.slice(0, 8) + '...'}
                      </Link>
                    </TableCell>
                    <TableCell>
                      <div className="flex items-center gap-2">
                        <StatusBadge status={subscriptionStatus(sub)} />
                        {(() => {
                          const paused = collectionPausedFlag(sub, resolveNow(sub.test_clock_id))
                          return paused && <StatusBadge status={paused} />
                        })()}
                      </div>
                    </TableCell>
                    <TableCell className="text-sm text-muted-foreground">
                      {sub.current_billing_period_display || '\u2014'}
                    </TableCell>
                    <TableCell className="text-sm text-muted-foreground">{formatDate(sub.created_at)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          ) : subsPage ? (
            <p className="text-sm text-muted-foreground text-center py-8">No subscriptions are using this plan yet</p>
          ) : (
            <p className="text-sm text-muted-foreground text-center py-8">Loading…</p>
          )}
          {subsTotal > subscriptions.length && (
            <p className="px-6 py-2 text-xs text-muted-foreground border-t border-border">Showing the first {subscriptions.length} of {subsTotal}.</p>
          )}
        </CardContent>
      </Card>

      {/* Edit Plan Dialog */}
      {showEdit && (
        <EditPlanDialog
          plan={plan}
          liveSubscriptions={liveSubscriptions}
          onClose={() => setShowEdit(false)}
          onSaved={() => {
            setShowEdit(false)
            queryClient.invalidateQueries({ queryKey: ['plan', id] })
            queryClient.invalidateQueries({ queryKey: ['plans'] })
            toast.success('Plan updated')
          }}
        />
      )}
    </Layout>
  )
}

function EditPlanDialog({ plan, liveSubscriptions, onClose, onSaved }: {
  plan: PlanDetail; liveSubscriptions: number; onClose: () => void; onSaved: () => void
}) {
  const locked = liveSubscriptions > 0
  const [basePrice, setBasePrice] = useState((plan.base_amount_cents / 100).toFixed(2))
  const [status, setStatus] = useState(plan.status)
  const [billTiming, setBillTiming] = useState<'in_arrears' | 'in_advance'>(plan.base_bill_timing ?? 'in_arrears')

  const form = useForm<EditPlanData>({
    resolver: zodResolver(editPlanSchema),
    defaultValues: { name: plan.name },
  })
  const { register, handleSubmit, watch, formState: { errors, isSubmitting } } = form

  const name = watch('name')
  const baseCents = Math.round(parseFloat(basePrice || '0') * 100)
  const fee = editPlanBaseFee({
    baseCents,
    chosenTiming: billTiming,
    storedTiming: plan.base_bill_timing ?? 'in_arrears',
    used: plan.used,
    interval: plan.billing_interval,
  })
  const timing = fee.timing
  const hasChanges = name !== plan.name ||
    baseCents !== plan.base_amount_cents ||
    status !== plan.status ||
    timing !== (plan.base_bill_timing ?? 'in_arrears')

  const onSubmit = handleSubmit(async (data) => {
    if (fee.baseError) return
    try {
      await api.updatePlan(plan.id, {
        name: data.name,
        status,
        ...(baseCents !== plan.base_amount_cents ? { base_amount_cents: baseCents } : {}),
        ...(timing !== plan.base_bill_timing ? { base_bill_timing: timing } : {}),
      })
      onSaved()
    } catch (err) {
      applyApiError(form, err, ['name'], { toastTitle: 'Failed to update plan' })
    }
  })

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Edit plan</DialogTitle>
        </DialogHeader>
        <form onSubmit={onSubmit} noValidate className="space-y-4">
          <div className="bg-muted rounded-lg px-4 py-3 flex items-center justify-between">
            <div>
              <p className="text-xs text-muted-foreground">Code</p>
              <p className="text-sm text-foreground font-mono mt-0.5">{plan.code}</p>
            </div>
            <div className="text-right">
              <p className="text-xs text-muted-foreground">Interval</p>
              <p className="text-sm text-foreground mt-0.5">{plan.billing_interval === 'yearly' ? 'Yearly' : 'Monthly'}</p>
            </div>
          </div>

          <div className="space-y-2">
            <Label htmlFor="name">Plan Name</Label>
            <Input id="name" maxLength={255} {...register('name')} />
            {errors.name && <p className="text-xs text-destructive">{errors.name.message}</p>}
          </div>

          {locked && (
            <Callout icon={Lock} title={PLAN_LOCKED_TITLE}>
              {planLockedSentence(liveSubscriptions)}
            </Callout>
          )}

          <div className="space-y-2">
            <Label htmlFor="basePrice">Base Price ({plan.currency.toUpperCase()})</Label>
            <Input
              id="basePrice"
              type="number"
              step="0.01"
              min={0}
              max={999999.99}
              value={basePrice}
              onChange={e => setBasePrice(e.target.value)}
              placeholder="49.00"
              disabled={locked}
              aria-invalid={fee.baseError ? true : undefined}
            />
            <p className="text-xs text-muted-foreground">{fee.baseHint}</p>
            {fee.baseError && <p className="text-xs text-destructive">{fee.baseError}</p>}
          </div>

          <div className="space-y-2">
            <Label>Base fee billed</Label>
            <Select items={BILL_TIMING_OPTIONS} value={timing} disabled={fee.timingDisabled} onValueChange={(v) => setBillTiming((v ?? 'in_arrears') as 'in_arrears' | 'in_advance')}>
              <SelectTrigger className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {BILL_TIMING_OPTIONS.map(o => (
                  <SelectItem key={o.value} value={o.value}>{o.label}</SelectItem>
                ))}
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground">{fee.timingHint}</p>
          </div>

          <div className="space-y-2">
            <Label>Status</Label>
            <Select items={PLAN_STATUS_OPTIONS} value={status} onValueChange={(v) => setStatus(v ?? '')}>
              <SelectTrigger className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {PLAN_STATUS_OPTIONS.map(o => (
                  <SelectItem key={o.value} value={o.value}>{o.label}</SelectItem>
                ))}
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground mt-1">Archived plans keep billing their existing subscriptions but can't be picked for new ones.</p>
          </div>

          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={isSubmitting || !hasChanges || fee.baseError !== null}>
              {isSubmitting ? (
                <><Loader2 size={14} className="animate-spin mr-2" />Saving...</>
              ) : hasChanges ? 'Save' : 'No changes'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
