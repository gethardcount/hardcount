import { useState } from 'react'
import { genericStatus } from '@/lib/statusLabels'
import { StatusBadge } from '@/components/StatusBadge'
import { usePageTitle } from '@/hooks/usePageTitle'
import { useParams, Link, useNavigate } from 'react-router-dom'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { api, formatCents, formatDateTime } from '@/lib/api'
import type { Meter } from '@/lib/api'
import { showApiError } from '@/lib/formErrors'
import { priceRate } from '@/lib/priceDisplay'
import { Labels } from '@/components/PlanPricesCard'
import {
  METER_AGGREGATIONS, METER_HAS_USAGE_SENTENCE, METER_NAME_UNIT_SENTENCE, METER_NO_USAGE_SENTENCE,
  meterAggregationHelp, meterAggregationLabel, meterHasUsageMessage,
} from '@/lib/meterLabels'
import { Layout } from '@/components/Layout'
import { LoadError } from '@/components/LoadError'
import { Callout } from '@/components/Callout'

import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from '@/components/ui/table'
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

import { Loader2, Lock, Pencil } from 'lucide-react'
import { CopyButton } from '@/components/CopyButton'
import { DetailBreadcrumb } from '@/components/DetailBreadcrumb'
import { DetailSkeleton } from '@/components/ui/DetailSkeleton'


export default function MeterDetailPage() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const queryClient = useQueryClient()

  const [editOpen, setEditOpen] = useState(false)

  const { data: meter, isLoading: meterLoading, error: meterError, refetch } = useQuery({
    queryKey: ['meter', id],
    queryFn: () => api.getMeter(id!),
    enabled: !!id,
  })

  const { data: plans } = useQuery({
    queryKey: ['meter-plans', id],
    queryFn: async () => {
      const res = await api.listPlans()
      return res.data.filter(p => (p.prices ?? []).some(pr => pr.meter_id === id))
    },
    enabled: !!id,
  })

  const loading = meterLoading

  usePageTitle(meter?.name)

  if (loading) {
    return (
      <Layout>
        <DetailSkeleton to="/pricing?tab=meters" parentLabel="Meters" />
      </Layout>
    )
  }

  if (meterError) {
    return (
      <Layout>
        <LoadError what="this meter" error={meterError} onRetry={() => refetch()} className="py-16" />
      </Layout>
    )
  }

  if (!meter) {
    return (
      <Layout>
        <p className="text-sm text-muted-foreground py-16 text-center">Meter not found</p>
      </Layout>
    )
  }

  return (
    <Layout>
      <DetailBreadcrumb to="/pricing?tab=meters" parentLabel="Meters" currentLabel={meter.name} />

      {/* Header */}
      <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-3 mb-6">
        <div className="min-w-0">
          <h1 className="text-2xl font-semibold text-foreground">{meter.name}</h1>
          <div className="flex items-center gap-2 mt-1">
            <span className="text-xs text-muted-foreground font-mono bg-muted px-2 py-0.5 rounded">{meter.id}</span>
            <CopyButton text={meter.id} />
          </div>
        </div>
        <Badge variant="secondary">{meterAggregationLabel(meter.aggregation)}</Badge>
      </div>

      {/* Properties */}
      <Card>
        <CardHeader>
          <div className="flex items-center justify-between gap-3">
            <CardTitle className="text-sm">Properties</CardTitle>
            <Button variant="outline" size="sm" onClick={() => setEditOpen(true)}>
              <Pencil size={14} className="mr-1.5" /> Edit
            </Button>
          </div>
        </CardHeader>
        <CardContent className="p-0">
          <div className="divide-y divide-border">
            <div className="flex items-center justify-between gap-3 px-6 py-3">
              <span className="text-sm text-muted-foreground">Event name</span>
              <span className="text-sm text-foreground font-mono break-all text-right">{meter.event_name}</span>
            </div>
            <div className="flex items-center justify-between px-6 py-3">
              <span className="text-sm text-muted-foreground">Unit</span>
              <span className="text-sm text-foreground">{meter.unit}</span>
            </div>
            <div className="px-6 py-3">
              <div className="flex items-center justify-between">
                <span className="text-sm text-muted-foreground">Aggregation</span>
                <Badge variant="secondary">{meterAggregationLabel(meter.aggregation)}</Badge>
              </div>
              <p className="text-xs text-muted-foreground mt-1.5 flex items-start gap-1.5" data-testid="meter-aggregation-lock">
                {meter.has_usage && <Lock size={12} className="mt-0.5 shrink-0" aria-hidden />}
                <span>
                  {meterAggregationHelp(meter.aggregation)}{' '}
                  {meter.has_usage ? METER_HAS_USAGE_SENTENCE : METER_NO_USAGE_SENTENCE}
                </span>
              </p>
            </div>
            <div className="flex items-center justify-between px-6 py-3">
              <span className="text-sm text-muted-foreground">Created</span>
              <span className="text-sm text-foreground">{formatDateTime(meter.created_at)}</span>
            </div>
            <div className="flex items-center justify-between px-6 py-3">
              <span className="text-sm text-muted-foreground">ID</span>
              <div className="flex items-center gap-2">
                <span className="text-sm text-foreground font-mono truncate max-w-xs" title={meter.id}>{meter.id}</span>
                <CopyButton text={meter.id} />
              </div>
            </div>
          </div>
        </CardContent>
      </Card>

      {/* Plans */}
      <Card className="mt-6">
        <CardHeader>
          <CardTitle className="text-sm">
            Plans that price this meter ({plans?.length ?? 0})
          </CardTitle>
          <p className="text-xs text-muted-foreground mt-0.5">
            A meter only counts. Each plan sets its own prices for the meter's usage; change them on the plan.
          </p>
        </CardHeader>
        <CardContent className="p-0">
          {plans && plans.length > 0 ? (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead>
                  <TableHead>Code</TableHead>
                  <TableHead>Prices</TableHead>
                  <TableHead className="text-right">Base Price</TableHead>
                  <TableHead>Status</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {plans.map(plan => (
                  <TableRow
                    key={plan.id}
                    className="cursor-pointer hover:bg-muted/50 transition-colors"
                    onClick={(e) => {
                      const target = e.target as HTMLElement
                      if (target.closest('button, a, input, select')) return
                      navigate(`/plans/${plan.id}`)
                    }}
                  >
                    <TableCell>
                      <Link to={`/plans/${plan.id}`} className="text-sm font-medium text-foreground hover:text-primary transition-colors">
                        {plan.name}
                      </Link>
                    </TableCell>
                    <TableCell className="text-sm text-muted-foreground font-mono">{plan.code}</TableCell>
                    <TableCell>
                      <div className="space-y-1">
                        {plan.prices.filter(pr => pr.meter_id === meter.id).map(pr => (
                          <div key={pr.id} className="flex flex-wrap items-center gap-2">
                            <Labels match={pr.dimension_match} />
                            <span className="text-sm text-foreground">{priceRate({ ...pr, currency: plan.currency }, meter.unit)}</span>
                          </div>
                        ))}
                      </div>
                    </TableCell>
                    <TableCell className="text-sm font-medium text-foreground text-right">{formatCents(plan.base_amount_cents, plan.currency)}</TableCell>
                    <TableCell><StatusBadge status={genericStatus(plan.status)} /></TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          ) : (
            <p className="text-sm text-muted-foreground text-center py-8">
              No plan prices this meter, so its usage isn't billed. Add a price for it on a plan.
            </p>
          )}
        </CardContent>
      </Card>

      {editOpen && (
        <EditMeterDialog
          meter={meter}
          onClose={() => setEditOpen(false)}
          onSaved={() => {
            setEditOpen(false)
            queryClient.invalidateQueries({ queryKey: ['meter', id] })
            queryClient.invalidateQueries({ queryKey: ['meters'] })
          }}
          onStale={() => queryClient.invalidateQueries({ queryKey: ['meter', id] })}
        />
      )}

    </Layout>
  )
}

// EditMeterDialog changes the meter's name, unit and aggregation. The
// aggregation is fixed once the meter has usage events (ADR-130 §2): the
// select is then read-only and the dialog says why before any edit. A refusal the
// page did not foresee (the first event arrived while the dialog was open)
// shows the server's sentence and reloads the meter.
function EditMeterDialog({
  meter,
  onClose,
  onSaved,
  onStale,
}: {
  meter: Meter
  onClose: () => void
  onSaved: () => void
  onStale: () => void
}) {
  const [name, setName] = useState(meter.name)
  const [unit, setUnit] = useState(meter.unit)
  const [aggregation, setAggregation] = useState(meter.aggregation)
  const [refusal, setRefusal] = useState<string | null>(null)
  const locked = !!meter.has_usage

  const saveMutation = useMutation({
    mutationFn: () => api.updateMeter(meter.id, {
      name: name.trim(),
      unit: unit.trim(),
      ...(aggregation !== meter.aggregation ? { aggregation } : {}),
    }),
    onMutate: () => setRefusal(null),
    onSuccess: () => {
      toast.success('Meter saved')
      onSaved()
    },
    onError: (err) => {
      const msg = meterHasUsageMessage(err)
      if (msg) {
        setRefusal(msg)
        setAggregation(meter.aggregation)
        onStale()
      } else showApiError(err, 'Failed to save meter')
    },
  })

  const changed = name.trim() !== meter.name || unit.trim() !== meter.unit || aggregation !== meter.aggregation
  const canSave = !!name.trim() && changed && !saveMutation.isPending

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Edit meter</DialogTitle>
          <DialogDescription>{METER_NAME_UNIT_SENTENCE}</DialogDescription>
        </DialogHeader>

        <div className="space-y-4">
          <div>
            <Label htmlFor="meter-name">Name</Label>
            <Input id="meter-name" value={name} onChange={e => setName(e.target.value)} maxLength={255} className="mt-1" />
          </div>
          <div>
            <Label htmlFor="meter-unit">Unit</Label>
            <Input id="meter-unit" value={unit} onChange={e => setUnit(e.target.value)} className="mt-1" />
          </div>
          <div>
            <Label>Aggregation</Label>
            <Select
              items={METER_AGGREGATIONS.map(a => ({ value: a.value, label: a.label }))}
              value={aggregation}
              onValueChange={(v) => setAggregation(v ?? meter.aggregation)}
              disabled={locked}
            >
              <SelectTrigger className="mt-1 w-full" aria-label="Aggregation">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {METER_AGGREGATIONS.map(a => (
                  <SelectItem key={a.value} value={a.value}>{a.label}</SelectItem>
                ))}
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground mt-1">{meterAggregationHelp(aggregation)}</p>
            <p className="text-xs text-muted-foreground mt-1 flex items-start gap-1.5">
              {locked && <Lock size={12} className="mt-0.5 shrink-0" aria-hidden />}
              <span>{locked ? METER_HAS_USAGE_SENTENCE : METER_NO_USAGE_SENTENCE}</span>
            </p>
          </div>
        </div>

        {refusal && (
          <Callout tone="amber" role="alert" title="Aggregation fixed">{refusal}</Callout>
        )}

        <DialogFooter>
          <Button variant="outline" onClick={onClose}>Cancel</Button>
          <Button onClick={() => saveMutation.mutate()} disabled={!canSave}>
            {saveMutation.isPending ? <Loader2 size={14} className="mr-1.5 animate-spin" /> : null}
            Save
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
