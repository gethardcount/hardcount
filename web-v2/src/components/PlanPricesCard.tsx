import { useState } from 'react'
import { Link } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { Loader2, Pencil, Plus, Trash2 } from 'lucide-react'
import { api, type Meter, type PlanDetail, type PlanPrice, type PriceWrite } from '@/lib/api'
import { showApiError } from '@/lib/formErrors'
import { priceRate } from '@/lib/priceDisplay'
import { planPricesNotice, priceRemoveSentence, priceWriteToast } from '@/lib/pricingCopy'
import { PriceShapeFields, buildPriceShape, usePriceShape } from '@/components/PriceShapeFields'
import { Combobox } from '@/components/Combobox'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from '@/components/ui/dialog'

// PlanPricesCard lists a plan's usage prices (ADR-130) and adds, edits and
// removes them. When a change applies is said before the operator saves:
// at once on a plan no one has used, from each subscriber's next period
// otherwise.

export function Labels({ match }: { match: Record<string, string> }) {
  const entries = Object.entries(match ?? {})
  if (entries.length === 0) return <span className="text-xs text-muted-foreground">All events</span>
  return (
    <div className="flex flex-wrap gap-1">
      {entries.map(([k, v]) => (
        <span key={k} className="inline-flex items-center text-xs font-mono bg-muted px-1.5 py-0.5 rounded">
          {k}=<span className="text-foreground">{v}</span>
        </span>
      ))}
    </div>
  )
}

export function PlanPricesCard({ plan, meters }: { plan: PlanDetail; meters: Meter[] }) {
  const queryClient = useQueryClient()
  const [adding, setAdding] = useState(false)
  const [editing, setEditing] = useState<PlanPrice | null>(null)
  const [removing, setRemoving] = useState<PlanPrice | null>(null)
  const meterOf = (id: string) => meters.find(m => m.id === id)

  const saved = (w: PriceWrite) => {
    queryClient.invalidateQueries({ queryKey: ['plan', plan.id] })
    queryClient.invalidateQueries({ queryKey: ['plans'] })
    toast.success(priceWriteToast(w))
  }

  return (
    <Card className="mt-6">
      <CardHeader>
        <div className="flex flex-wrap items-center justify-between gap-2">
          <CardTitle className="text-sm">Usage prices ({plan.prices.length})</CardTitle>
          <Button size="sm" onClick={() => setAdding(true)} disabled={meters.length === 0}>
            <Plus size={14} className="mr-1.5" />
            Add price
          </Button>
        </div>
        <p className="text-xs text-muted-foreground mt-1" data-testid="plan-prices-notice">{planPricesNotice(plan.used)}</p>
      </CardHeader>
      <CardContent className="p-0">
        {plan.prices.length > 0 ? (
          <div className="divide-y divide-border border-t border-border">
            {plan.prices.map(p => {
              const m = meterOf(p.meter_id)
              return (
                <div key={p.id} data-testid="plan-price-row" className="flex items-start justify-between gap-3 px-6 py-3">
                  <div className="min-w-0 space-y-1">
                    <p className="flex flex-wrap items-baseline gap-x-2">
                      <Link to={`/meters/${p.meter_id}`} className="text-sm font-medium text-foreground hover:text-primary transition-colors">
                        {m?.name ?? p.meter_id}
                      </Link>
                      {m && <span className="text-xs text-muted-foreground font-mono">{m.event_name}</span>}
                    </p>
                    <Labels match={p.dimension_match} />
                    <p className="text-sm text-foreground">{priceRate({ ...p, currency: plan.currency }, m?.unit ?? '')}</p>
                  </div>
                  <div className="flex shrink-0 items-center">
                    <Button variant="ghost" size="icon" className="h-8 w-8 text-muted-foreground" aria-label="Edit price" onClick={() => setEditing(p)}>
                      <Pencil size={14} />
                    </Button>
                    <Button variant="ghost" size="icon" className="h-8 w-8 text-muted-foreground hover:text-destructive" aria-label="Remove price" onClick={() => setRemoving(p)}>
                      <Trash2 size={14} />
                    </Button>
                  </div>
                </div>
              )
            })}
          </div>
        ) : (
          <p className="text-sm text-muted-foreground text-center py-8 px-6">
            {meters.length === 0
              ? <>No prices. Create a meter on the <Link to="/pricing?tab=meters" className="text-primary hover:underline">Pricing page</Link> first, then price it here.</>
              : 'No usage prices — this plan charges only its base fee. Add a price to bill a meter\'s usage.'}
          </p>
        )}
      </CardContent>

      {adding && (
        <AddPriceDialog plan={plan} meters={meters} onClose={() => setAdding(false)} onSaved={(w) => { setAdding(false); saved(w) }} />
      )}
      {editing && (
        <EditPriceDialog plan={plan} price={editing} meter={meterOf(editing.meter_id)} onClose={() => setEditing(null)} onSaved={(w) => { setEditing(null); saved(w) }} />
      )}
      {removing && (
        <RemovePriceDialog plan={plan} price={removing} meter={meterOf(removing.meter_id)} onClose={() => setRemoving(null)} onRemoved={(w) => { setRemoving(null); saved(w) }} />
      )}
    </Card>
  )
}

interface LabelRow { key: string; value: string }

function AddPriceDialog({ plan, meters, onClose, onSaved }: { plan: PlanDetail; meters: Meter[]; onClose: () => void; onSaved: (w: PriceWrite) => void }) {
  const [meterId, setMeterId] = useState('')
  const [labels, setLabels] = useState<LabelRow[]>([])
  const shape = usePriceShape()
  const [formError, setFormError] = useState('')
  const [saving, setSaving] = useState(false)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setFormError('')
    const built = buildPriceShape(shape)
    if ('error' in built) { setFormError(built.error); return }
    const match: Record<string, string> = {}
    for (const row of labels) {
      const k = row.key.trim()
      if (!k) continue
      match[k] = row.value.trim()
    }
    setSaving(true)
    try {
      onSaved(await api.addPlanPrice(plan.id, { meter_id: meterId, dimension_match: match, ...built.shape }))
    } catch (err) {
      showApiError(err, 'Failed to add price')
    } finally {
      setSaving(false)
    }
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Add price</DialogTitle>
          <DialogDescription>{planPricesNotice(plan.used)}</DialogDescription>
        </DialogHeader>
        <form onSubmit={submit} noValidate className="space-y-4">
          <div>
            <Label>Meter</Label>
            <div className="mt-1">
              <Combobox
                value={meterId}
                onChange={(v) => setMeterId(v ?? '')}
                options={meters.map(m => ({ value: m.id, label: `${m.name} (${m.event_name})`, keywords: [m.event_name] }))}
                placeholder="Select a meter…"
                emptyMessage="No meter matches that search."
                triggerClassName="w-full"
              />
            </div>
          </div>

          <div>
            <Label>Only for events with these labels</Label>
            <div className="space-y-2 mt-1">
              {labels.map((row, i) => (
                <div key={i} className="flex items-center gap-2">
                  <Input aria-label={`Label ${i + 1} key`} placeholder="key (e.g. model)" value={row.key} className="font-mono text-sm"
                    onChange={e => setLabels(rs => rs.map((r, j) => j === i ? { ...r, key: e.target.value } : r))} />
                  <span className="text-muted-foreground">=</span>
                  <Input aria-label={`Label ${i + 1} value`} placeholder="value (e.g. gpt-4o)" value={row.value} className="font-mono text-sm"
                    onChange={e => setLabels(rs => rs.map((r, j) => j === i ? { ...r, value: e.target.value } : r))} />
                  <Button type="button" variant="ghost" size="icon" className="h-9 w-9 shrink-0 text-muted-foreground" aria-label={`Remove label ${i + 1}`}
                    onClick={() => setLabels(rs => rs.filter((_, j) => j !== i))}>
                    <Trash2 size={14} />
                  </Button>
                </div>
              ))}
            </div>
            <Button type="button" variant="outline" size="sm" className="mt-2" onClick={() => setLabels(rs => [...rs, { key: '', value: '' }])}>
              <Plus size={14} className="mr-1.5" /> Add label
            </Button>
            <p className="text-xs text-muted-foreground mt-1">
              No labels takes every event of the meter. When an event fits more than one price, the price with the most labels takes it.
            </p>
          </div>

          <PriceShapeFields f={shape} currency={plan.currency} idPrefix="add-price" />

          {formError && <p className="text-sm text-destructive">{formError}</p>}

          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={saving || !meterId}>
              {saving ? <><Loader2 size={14} className="animate-spin mr-2" /> Saving...</> : 'Add price'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function EditPriceDialog({ plan, price, meter, onClose, onSaved }: { plan: PlanDetail; price: PlanPrice; meter?: Meter; onClose: () => void; onSaved: (w: PriceWrite) => void }) {
  const shape = usePriceShape(price)
  const [formError, setFormError] = useState('')
  const [saving, setSaving] = useState(false)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setFormError('')
    const built = buildPriceShape(shape)
    if ('error' in built) { setFormError(built.error); return }
    setSaving(true)
    try {
      onSaved(await api.updatePlanPrice(plan.id, price.id, built.shape))
    } catch (err) {
      showApiError(err, 'Failed to change price')
    } finally {
      setSaving(false)
    }
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Change price</DialogTitle>
          <DialogDescription>{planPricesNotice(plan.used)}</DialogDescription>
        </DialogHeader>
        <form onSubmit={submit} noValidate className="space-y-4">
          <div className="bg-muted rounded-lg px-4 py-3 space-y-1">
            <p className="text-sm text-foreground">{meter?.name ?? price.meter_id}</p>
            <Labels match={price.dimension_match} />
            <p className="text-xs text-muted-foreground">The meter and labels can't change. For other events, add another price.</p>
          </div>
          <PriceShapeFields f={shape} currency={plan.currency} idPrefix="edit-price" />
          {formError && <p className="text-sm text-destructive">{formError}</p>}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={saving}>
              {saving ? <><Loader2 size={14} className="animate-spin mr-2" /> Saving...</> : 'Save'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function RemovePriceDialog({ plan, price, meter, onClose, onRemoved }: { plan: PlanDetail; price: PlanPrice; meter?: Meter; onClose: () => void; onRemoved: (w: PriceWrite) => void }) {
  const [removing, setRemoving] = useState(false)
  // The overrides the removal ends, named before the operator confirms.
  const { data: overrides, isError } = useQuery({
    queryKey: ['price-overrides', 'price', price.id],
    queryFn: () => api.listPriceOverrides({ priceId: price.id }),
    select: (r) => r.data.filter(o => o.active),
  })

  const confirm = async () => {
    setRemoving(true)
    try {
      onRemoved(await api.removePlanPrice(plan.id, price.id))
    } catch (err) {
      showApiError(err, 'Failed to remove price')
      setRemoving(false)
    }
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Remove price?</DialogTitle>
          <DialogDescription>
            {meter?.name ?? price.meter_id} · {priceRate({ ...price, currency: plan.currency }, meter?.unit ?? '')}
          </DialogDescription>
        </DialogHeader>
        <Labels match={price.dimension_match} />
        <p className="text-sm text-muted-foreground">{priceRemoveSentence(plan.used, isError ? null : overrides?.length ?? 0)}</p>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
          <Button type="button" variant="destructive" onClick={confirm} disabled={removing || (overrides === undefined && !isError)}>
            {removing ? <><Loader2 size={14} className="animate-spin mr-2" /> Removing...</> : 'Remove price'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
