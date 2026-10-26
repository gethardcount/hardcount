import { useState } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import {
  api,
  formatCents,
  formatDate,
  getCurrencySymbol,
  getActiveCurrency,
} from '@/lib/api'
import { Layout } from '@/components/Layout'
import { LoadError } from '@/components/LoadError'
import { METER_AGGREGATIONS, meterAggregationHelp, meterAggregationLabel } from '@/lib/meterLabels'
import { showApiError } from '@/lib/formErrors'
import { statusBadgeVariant } from '@/lib/status'

import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent } from '@/components/ui/card'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { TableSkeleton } from '@/components/ui/TableSkeleton'

import { Plus, Loader2, Package, Gauge } from 'lucide-react'
import { EmptyState } from '@/components/EmptyState'

// Static option sets for the pricing selects. Each is the single source of
// truth for BOTH the dropdown <SelectItem>s AND the Base UI `items` prop.
// Base UI's <SelectValue> renders the raw value in the trigger unless `items`
// maps value→label — the app-wide cause of "trigger shows the code, dropdown
// shows the name".
const INTERVAL_OPTIONS = [
  { value: 'monthly', label: 'Monthly' },
  { value: 'yearly', label: 'Yearly' },
]
const BILL_TIMING_OPTIONS = [
  { value: 'in_arrears', label: 'At end of period (usage-first)' },
  { value: 'in_advance', label: 'At start of period (platform fee + usage)' },
]

function badgeVariant(val: string): 'success' | 'info' | 'secondary' | 'outline' {
  switch (val) {
    case 'active': return 'success'
    case 'monthly': return 'info'
    case 'yearly': return 'info'
    default: return 'secondary'
  }
}

export default function PricingPage() {
  usePageTitle('Pricing')
  const [searchParams, setSearchParams] = useSearchParams()
  const tab = (searchParams.get('tab') === 'meters' ? 'meters' : 'plans') as 'plans' | 'meters'
  const setTab = (t: string) => setSearchParams(t === 'plans' ? {} : { tab: t })
  const [createFor, setCreateFor] = useState<'plans' | 'meters' | null>(null)
  const navigate = useNavigate()
  const queryClient = useQueryClient()

  const { data: plansData, isLoading: plansLoading, error: plansError } = useQuery({
    queryKey: ['plans'],
    queryFn: () => api.listPlans(),
  })
  const { data: metersData, isLoading: metersLoading, error: metersError } = useQuery({
    queryKey: ['meters'],
    queryFn: () => api.listMeters(),
  })
  const plans = plansData?.data ?? []
  const meters = metersData?.data ?? []
  // A meter is billed by the plans that have a price for it (ADR-130).
  const plansPricing = (meterID: string): number =>
    plans.filter(p => (p.prices ?? []).some(pr => pr.meter_id === meterID)).length

  const loading = plansLoading || metersLoading
  const error = plansError || metersError
  const showCounts = !loading && !error

  const showAdd =
    (tab === 'plans' && plans.length > 0) ||
    (tab === 'meters' && meters.length > 0)

  return (
    <Layout>
      <div className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
        <div className="max-w-3xl">
          <h1 className="text-2xl font-semibold text-foreground">Pricing</h1>
          <p className="text-sm text-muted-foreground mt-1">
            Set what you charge. A meter counts something your customers use, like API calls or tokens.
            A plan has a base fee and the prices of its meters' usage. To bill a customer, you put
            them on a plan with a subscription.
          </p>
          <p className="text-sm text-muted-foreground mt-2">
            New here? Start with a plan, or install a{' '}
            <Link to="/recipes" className="text-primary hover:underline">ready-made recipe</Link>.
          </p>
        </div>
        {showAdd && (
          <Button size="sm" className="shrink-0 self-start" onClick={() => setCreateFor(tab)}>
            <Plus size={16} className="mr-2" />
            {tab === 'plans' ? 'Add Plan' : 'Add Meter'}
          </Button>
        )}
      </div>

      <Tabs value={tab} onValueChange={setTab} className="mt-6">
        <TabsList>
          {/* Counts only once the lists have loaded: "(0)" over a load
              error or a skeleton claims an empty list nobody has seen. */}
          <TabsTrigger value="plans">Plans{showCounts ? ` (${plans.length})` : ''}</TabsTrigger>
          <TabsTrigger value="meters">Meters{showCounts ? ` (${meters.length})` : ''}</TabsTrigger>
        </TabsList>

        <Card className="mt-4">
          <CardContent className="p-0">
            {error ? (
              <LoadError
                what="pricing"
                error={error}
                onRetry={() => {
                  // A failure may have come from any of the tab queries —
                  // retrying only one leaves the button dead for the rest.
                  for (const key of ['plans', 'meters']) {
                    queryClient.invalidateQueries({ queryKey: [key] })
                  }
                }}
              />
            ) : loading ? (
              <TableSkeleton columns={6} />
            ) : (
              <>
                <TabsContent value="plans">
                  {plans.length === 0 ? (
                    <EmptyState
                      icon={Package}
                      title="No plans yet"
                      description="Create a plan to define pricing and intervals for your product."
                      action={{
                        label: 'Add Plan',
                        icon: Plus,
                        onClick: () => setCreateFor('plans'),
                      }}
                    />
                  ) : (
                    <Table>
                      <TableHeader>
                        <TableRow>
                          <TableHead>Name</TableHead>
                          <TableHead>Code</TableHead>
                          <TableHead>Interval</TableHead>
                          <TableHead>Status</TableHead>
                          <TableHead className="text-right">Base Price</TableHead>
                          <TableHead className="text-right">Prices</TableHead>
                        </TableRow>
                      </TableHeader>
                      <TableBody>
                        {plans.map(p => (
                          <TableRow
                            key={p.id}
                            className="cursor-pointer hover:bg-muted/50 transition-colors"
                            onClick={(e) => {
                              const target = e.target as HTMLElement
                              if (target.closest('button, a, input, select')) return
                              navigate(`/plans/${p.id}`)
                            }}
                          >
                            <TableCell>
                              <Link to={`/plans/${p.id}`} className="font-medium text-foreground hover:text-primary transition-colors">
                                {p.name}
                              </Link>
                            </TableCell>
                            <TableCell className="font-mono text-muted-foreground">{p.code}</TableCell>
                            <TableCell><Badge variant={badgeVariant(p.billing_interval)}>{p.billing_interval}</Badge></TableCell>
                            <TableCell><Badge variant={statusBadgeVariant(p.status)}>{p.status}</Badge></TableCell>
                            <TableCell className="text-right font-medium">{formatCents(p.base_amount_cents, p.currency)}</TableCell>
                            <TableCell className="text-right text-muted-foreground">{p.prices?.length || 0}</TableCell>
                          </TableRow>
                        ))}
                      </TableBody>
                    </Table>
                  )}
                </TabsContent>

                <TabsContent value="meters">
                  {meters.length === 0 ? (
                    <EmptyState
                      icon={Gauge}
                      title="No meters yet"
                      description="Meters track consumption of billable units like API calls, messages, or compute time."
                      action={{
                        label: 'Add Meter',
                        icon: Plus,
                        onClick: () => setCreateFor('meters'),
                      }}
                    />
                  ) : (
                    <Table>
                      <TableHeader>
                        <TableRow>
                          <TableHead>Name</TableHead>
                          <TableHead>Event name</TableHead>
                          <TableHead>Unit</TableHead>
                          <TableHead>Priced by</TableHead>
                          <TableHead>Aggregation</TableHead>
                          <TableHead>Created</TableHead>
                        </TableRow>
                      </TableHeader>
                      <TableBody>
                        {meters.map(m => (
                          <TableRow
                            key={m.id}
                            className="cursor-pointer hover:bg-muted/50 transition-colors"
                            onClick={(e) => {
                              const target = e.target as HTMLElement
                              if (target.closest('button, a, input, select')) return
                              navigate(`/meters/${m.id}`)
                            }}
                          >
                            <TableCell>
                              <Link to={`/meters/${m.id}`} className="font-medium text-foreground hover:text-primary transition-colors">
                                {m.name}
                              </Link>
                            </TableCell>
                            <TableCell className="font-mono text-muted-foreground">{m.event_name}</TableCell>
                            <TableCell className="text-muted-foreground">{m.unit}</TableCell>
                            {(() => {
                              const n = plansPricing(m.id)
                              return (
                                <TableCell className="text-sm">
                                  {n > 0 ? (
                                    <span className="text-muted-foreground">{n} plan{n !== 1 ? 's' : ''}</span>
                                  ) : (
                                    <span className="text-amber-500">No plan prices it — usage won't be billed</span>
                                  )}
                                </TableCell>
                              )
                            })()}
                            <TableCell><Badge variant={badgeVariant(m.aggregation)}>{meterAggregationLabel(m.aggregation)}</Badge></TableCell>
                            <TableCell className="text-muted-foreground">{formatDate(m.created_at)}</TableCell>
                          </TableRow>
                        ))}
                      </TableBody>
                    </Table>
                  )}
                </TabsContent>

              </>
            )}
          </CardContent>
        </Card>
      </Tabs>

      {createFor === 'meters' && (
        <CreateMeterDialog
          onClose={() => setCreateFor(null)}
          onCreated={(meterId) => {
            setCreateFor(null)
            queryClient.invalidateQueries({ queryKey: ['meters'] })
            toast.success('Meter created')
            // Land on the meter page; its prices are set on plans.
            if (meterId) navigate(`/meters/${meterId}`)
          }}
        />
      )}
      {createFor === 'plans' && (
        <CreatePlanDialog
          onClose={() => setCreateFor(null)}
          onCreated={(planId) => {
            setCreateFor(null)
            queryClient.invalidateQueries({ queryKey: ['plans'] })
            toast.success('Plan created')
            // Land on the plan page, where its usage prices are added.
            navigate(`/plans/${planId}`)
          }}
        />
      )}
    </Layout>
  )
}

/* ─── Create Meter Dialog ─── */

function CreateMeterDialog({ onClose, onCreated }: { onClose: () => void; onCreated: (meterId?: string) => void }) {
  const [form, setForm] = useState({ event_name: '', name: '', unit: 'unit', aggregation: 'sum' })
  const [saving, setSaving] = useState(false)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setSaving(true)
    try {
      const created = await api.createMeter(form)
      onCreated(created?.id)
    } catch (err) {
      showApiError(err, 'Failed to create meter')
    } finally {
      setSaving(false)
    }
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Create meter</DialogTitle>
          <DialogDescription>A meter counts one thing your customers use, like API calls or tokens.</DialogDescription>
        </DialogHeader>
        <form onSubmit={submit} noValidate className="space-y-4">
          <div>
            <Label>Name</Label>
            <Input value={form.name} onChange={e => setForm(f => ({ ...f, name: e.target.value }))}
              placeholder="API Calls" maxLength={255} className="mt-1" />
          </div>
          <div>
            <Label>Event name</Label>
            <Input value={form.event_name} onChange={e => setForm(f => ({ ...f, event_name: e.target.value }))}
              placeholder="api_calls" maxLength={100} className="mt-1 font-mono" />
            <p className="text-xs text-muted-foreground mt-1">
              The name your app sends with each usage event, e.g. api_calls.
            </p>
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div>
              <Label>Unit Label</Label>
              <Input value={form.unit} onChange={e => setForm(f => ({ ...f, unit: e.target.value }))}
                placeholder="unit" className="mt-1" />
              <p className="text-xs text-muted-foreground mt-1">e.g. call, request, GB</p>
            </div>
            <div>
              <Label>Aggregation</Label>
              <Select items={METER_AGGREGATIONS.map(a => ({ value: a.value, label: a.label }))} value={form.aggregation} onValueChange={v => setForm(f => ({ ...f, aggregation: v ?? '' }))}>
                <SelectTrigger className="w-full mt-1">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {METER_AGGREGATIONS.map(o => (
                    <SelectItem key={o.value} value={o.value}>{o.label}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <p className="text-xs text-muted-foreground mt-1">{meterAggregationHelp(form.aggregation)} Fixed once the meter has a usage event.</p>
            </div>
          </div>
          <p className="text-xs text-muted-foreground">
            A meter only counts. Its usage is billed by the plans that add a price for it.
          </p>

          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={saving}>
              {saving ? <><Loader2 size={14} className="animate-spin mr-2" /> Saving...</> : 'Create meter'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

/* ─── Create Plan Dialog ─── */

function CreatePlanDialog({ onClose, onCreated }: { onClose: () => void; onCreated: (planId: string) => void }) {
  const [name, setName] = useState('')
  const [code, setCode] = useState('')
  const [interval, setInterval] = useState('monthly')
  const [basePrice, setBasePrice] = useState('')
  const [billTiming, setBillTiming] = useState<'in_arrears' | 'in_advance'>('in_arrears')
  const [saving, setSaving] = useState(false)

  // The plan's prices take its currency. The Base Price label reads the same
  // value, so it names the currency the plan is created in.
  const planCurrency = getActiveCurrency()
  const baseCents = Math.round(parseFloat(basePrice || '0') * 100)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setSaving(true)
    try {
      const created = await api.createPlan({
        name,
        code,
        currency: planCurrency,
        billing_interval: interval,
        base_amount_cents: baseCents,
        // Required with a base fee; a plan with none is billed at period end.
        ...(baseCents > 0 ? { base_bill_timing: billTiming } : {}),
      })
      onCreated(created.id)
    } catch (err) {
      showApiError(err, 'Failed to create plan')
    } finally {
      setSaving(false)
    }
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Create plan</DialogTitle>
          <DialogDescription>A plan is what a customer subscribes to: a base fee, usage prices, or both. You add its usage prices on the plan's page.</DialogDescription>
        </DialogHeader>
        <form onSubmit={submit} noValidate className="space-y-4">
          <div>
            <Label>Plan Name</Label>
            <Input value={name} onChange={e => setName(e.target.value)}
              placeholder="Pro Plan" maxLength={255} className="mt-1" />
          </div>
          <div>
            <Label htmlFor="plan-code">Code</Label>
            <Input id="plan-code" value={code} onChange={e => setCode(e.target.value)}
              placeholder="pro" maxLength={100} className="mt-1 font-mono" />
            <p className="text-xs text-muted-foreground mt-1">Unique identifier used in API calls</p>
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div>
              <Label>Billing Interval</Label>
              <Select items={INTERVAL_OPTIONS} value={interval} onValueChange={(v) => setInterval(v ?? '')}>
                <SelectTrigger className="w-full mt-1">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {INTERVAL_OPTIONS.map(o => (
                    <SelectItem key={o.value} value={o.value}>{o.label}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>
          <div>
            <Label>Base Price ({getCurrencySymbol(planCurrency)})</Label>
            <Input type="number" step="0.01" min={0} max={999999.99} value={basePrice}
              onChange={e => setBasePrice(e.target.value)} placeholder="49.00" className="mt-1" />
            <p className="text-xs text-muted-foreground mt-1">Fixed {interval} charge before usage fees</p>
          </div>
          {baseCents > 0 && (
            <div>
              <Label>Base fee billed</Label>
              <Select items={BILL_TIMING_OPTIONS} value={billTiming} onValueChange={(v) => setBillTiming((v ?? 'in_arrears') as 'in_arrears' | 'in_advance')}>
                <SelectTrigger className="w-full mt-1">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {BILL_TIMING_OPTIONS.map(o => (
                    <SelectItem key={o.value} value={o.value}>{o.label}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <p className="text-xs text-muted-foreground mt-1">
                {billTiming === 'in_advance'
                  ? 'Customer is charged the base fee on day 1 of each period; usage settles at period end.'
                  : 'Customer is charged the base fee plus any usage at the end of each period.'}
              </p>
            </div>
          )}

          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={saving}>
              {saving ? <><Loader2 size={14} className="animate-spin mr-2" /> Saving...</> : 'Create plan'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
