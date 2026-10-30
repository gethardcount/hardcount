import { useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { Loader2, Pencil, Plus, Trash2 } from 'lucide-react'
import { api, type Meter, type PlanDetail, type PlanPrice, type PriceWrite } from '@/lib/api'
import { showApiError } from '@/lib/formErrors'
import { money, priceRate, unitScale } from '@/lib/priceDisplay'
import { buildRateCard, rowMatches } from '@/lib/rateCard'
import { planPricesNotice, priceRemoveSentence, priceWriteToast } from '@/lib/pricingCopy'
import { labelRowsToMatch, type LabelRow, type LabelRowError } from '@/lib/dimensionLabels'
import { buildPriceShape } from '@/lib/priceShapeForm'
import { PriceShapeFields, usePriceShape } from '@/components/PriceShapeFields'
import { Combobox } from '@/components/Combobox'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from '@/components/ui/dialog'

// PlanPricesCard lists a plan's usage prices (ADR-130) and adds, edits and
// removes them. When a change applies is said before the operator saves:
// at once on a plan no one has used, from each subscriber's next period
// otherwise.
//
// A meter whose prices fit a grid reads as a rate card (lib/rateCard): an AI
// plan's 125 prices are one row per model with a column per token type,
// about 30 rows instead of 125 blocks. Other prices keep one row each. A
// search box appears once there are more prices than a glance takes in.

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
  const [query, setQuery] = useState('')
  const meterOf = (id: string) => meters.find(m => m.id === id)
  // Prices grouped by meter, in the plan's order.
  const groups = useMemo(() => {
    const out: { meterId: string; prices: PlanPrice[] }[] = []
    for (const p of plan.prices) {
      const g = out.find(x => x.meterId === p.meter_id)
      if (g) g.prices.push(p)
      else out.push({ meterId: p.meter_id, prices: [p] })
    }
    return out
  }, [plan.prices])
  const searchable = plan.prices.length > 8
  const q = searchable ? query : ''

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
          <div className="flex items-center gap-2">
            {searchable && (
              <Input
                type="search"
                aria-label="Search prices"
                placeholder="Search model or label…"
                value={query}
                onChange={e => setQuery(e.target.value)}
                className="h-8 w-48 sm:w-56"
              />
            )}
            <Button size="sm" onClick={() => setAdding(true)} disabled={meters.length === 0}>
              <Plus size={14} className="mr-1.5" />
              Add price
            </Button>
          </div>
        </div>
        <p className="text-xs text-muted-foreground mt-1" data-testid="plan-prices-notice">{planPricesNotice(plan.used)}</p>
      </CardHeader>
      <CardContent className="p-0">
        {plan.prices.length > 0 ? (
          <div className="border-t border-border">
            {groups.map(g => {
              const m = meterOf(g.meterId)
              const card = buildRateCard(g.prices)
              if (card) return <RateCardTable key={g.meterId} card={card} meter={m} meterId={g.meterId} currency={plan.currency} query={q} onEdit={setEditing} />
              const shown = g.prices.filter(p => rowMatches(p.dimension_match ?? {}, m?.name ?? '', q))
              if (shown.length === 0) return null
              return (
                <div key={g.meterId} className="divide-y divide-border border-b border-border last:border-b-0">
                  {shown.map(p => (
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
                  ))}
                </div>
              )
            })}
            {q && !plan.prices.some(p => rowMatches(p.dimension_match ?? {}, meterOf(p.meter_id)?.name ?? '', q)) && (
              <p className="text-sm text-muted-foreground text-center py-8 px-6">No price matches "{q}".</p>
            )}
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
        <EditPriceDialog plan={plan} price={editing} meter={meterOf(editing.meter_id)} onClose={() => setEditing(null)} onSaved={(w) => { setEditing(null); saved(w) }}
          onRemove={() => { setRemoving(editing); setEditing(null) }} />
      )}
      {removing && (
        <RemovePriceDialog plan={plan} price={removing} meter={meterOf(removing.meter_id)} onClose={() => setRemoving(null)} onRemoved={(w) => { setRemoving(null); saved(w) }} />
      )}
    </Card>
  )
}

// RateCardTable is one meter's prices as a grid: the row labels in one
// column (the first label as the name, any other as a small tag, so a label
// few rows carry does not cost a column of dashes), then one column per value
// of the column label. The label column stays put while a phone scrolls the
// prices sideways. A cell is the per-unit rate in the
// meter's display unit (per 1M tokens for tokens); clicking it opens the
// price, where it can be changed or removed. An empty cell has no price.
function RateCardTable({ card, meter, meterId, currency, query, onEdit }: {
  card: NonNullable<ReturnType<typeof buildRateCard<PlanPrice>>>
  meter?: Meter
  meterId: string
  currency: string
  query: string
  onEdit: (p: PlanPrice) => void
}) {
  const { scale, per } = unitScale(meter?.unit ?? '')
  const rows = card.rows.filter(r => rowMatches(r.labels, meter?.name ?? '', query))
  if (rows.length === 0) return null
  return (
    <div data-testid="plan-rate-card" className="border-b border-border last:border-b-0">
      <p className="flex flex-wrap items-baseline gap-x-2 px-6 pt-3 pb-2">
        <Link to={`/meters/${meterId}`} className="text-sm font-medium text-foreground hover:text-primary transition-colors">
          {meter?.name ?? meterId}
        </Link>
        {meter && <span className="text-xs text-muted-foreground font-mono">{meter.event_name}</span>}
        <span className="text-xs text-muted-foreground">
          Price per {per}, by {card.columnKey}. Select a price to change or remove it.
        </span>
      </p>
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead className="sticky left-0 z-10 bg-card px-3 sm:px-6 font-mono text-xs">{card.rowKeys[0] ?? ''}</TableHead>
            {card.columns.map(c => <TableHead key={c} className="text-right font-mono text-xs">{c}</TableHead>)}
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map(r => (
            <TableRow key={JSON.stringify(r.labels)}>
              {/* On a phone the pinned column is capped and its tags wrap, so the
                  prices have room to scroll into view beside it. */}
              <TableCell className="sticky left-0 z-10 bg-card px-3 sm:px-6 font-mono text-xs w-40 min-w-40 max-w-40 whitespace-normal sm:w-auto sm:min-w-0 sm:max-w-none sm:whitespace-nowrap">
                {card.rowKeys[0] !== undefined && r.labels[card.rowKeys[0]]}
                {card.rowKeys.slice(1).filter(k => r.labels[k] !== undefined).map(k => (
                  <span key={k} className="mt-1 flex w-fit max-w-full items-center break-all rounded bg-muted px-1.5 py-0.5 text-[11px] text-muted-foreground sm:ml-2 sm:mt-0 sm:inline-flex sm:break-normal">
                    {k}={r.labels[k]}
                  </span>
                ))}
              </TableCell>
              {card.columns.map(c => {
                const p = r.cells[c]
                return (
                  <TableCell key={c} className="text-right">
                    {p ? (
                      <button
                        type="button"
                        onClick={() => onEdit(p)}
                        aria-label={`Change price: ${Object.entries(p.dimension_match ?? {}).map(([k, v]) => `${k}=${v}`).join(', ')}`}
                        className="font-mono tabular-nums text-sm text-foreground hover:text-primary hover:underline underline-offset-2"
                      >
                        {money(p.unit_amount_cents ?? '0', scale, currency)}
                      </button>
                    ) : <span className="text-muted-foreground" aria-label="No price">—</span>}
                  </TableCell>
                )
              })}
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  )
}

function AddPriceDialog({ plan, meters, onClose, onSaved }: { plan: PlanDetail; meters: Meter[]; onClose: () => void; onSaved: (w: PriceWrite) => void }) {
  const [meterId, setMeterId] = useState('')
  const [labels, setLabelsRaw] = useState<LabelRow[]>([])
  const [labelErrors, setLabelErrors] = useState<Record<number, LabelRowError>>({})
  // Any edit to the labels clears their refusals.
  const setLabels: typeof setLabelsRaw = (v) => { setLabelErrors({}); setLabelsRaw(v) }
  const shape = usePriceShape()
  const [saving, setSaving] = useState(false)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    // Both checks run, so every refusal shows at once.
    const read = labelRowsToMatch(labels)
    const built = buildPriceShape(shape)
    setLabelErrors('errors' in read ? read.errors : {})
    shape.setError('error' in built ? built.error : null)
    if ('errors' in read || 'error' in built) return
    setSaving(true)
    try {
      onSaved(await api.addPlanPrice(plan.id, { meter_id: meterId, dimension_match: read.match, ...built.shape }))
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
                <div key={i}>
                  <div className="flex items-center gap-2">
                    <Input aria-label={`Label ${i + 1} key`} placeholder="key (e.g. model)" value={row.key} className="font-mono text-sm"
                      aria-invalid={labelErrors[i]?.key ? true : undefined}
                      onChange={e => setLabels(rs => rs.map((r, j) => j === i ? { ...r, key: e.target.value } : r))} />
                    <span className="text-muted-foreground">=</span>
                    <Input aria-label={`Label ${i + 1} value`} placeholder="value (e.g. gpt-4o)" value={row.value} className="font-mono text-sm"
                      aria-invalid={labelErrors[i]?.value ? true : undefined}
                      onChange={e => setLabels(rs => rs.map((r, j) => j === i ? { ...r, value: e.target.value } : r))} />
                    <Button type="button" variant="ghost" size="icon" className="h-9 w-9 shrink-0 text-muted-foreground" aria-label={`Remove label ${i + 1}`}
                      onClick={() => setLabels(rs => rs.filter((_, j) => j !== i))}>
                      <Trash2 size={14} />
                    </Button>
                  </div>
                  {labelErrors[i]?.key && <p className="text-xs text-destructive mt-1">{labelErrors[i].key}</p>}
                  {labelErrors[i]?.value && <p className="text-xs text-destructive mt-1">{labelErrors[i].value}</p>}
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

function EditPriceDialog({ plan, price, meter, onClose, onSaved, onRemove }: { plan: PlanDetail; price: PlanPrice; meter?: Meter; onClose: () => void; onSaved: (w: PriceWrite) => void; onRemove: () => void }) {
  const shape = usePriceShape(price)
  const [saving, setSaving] = useState(false)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    const built = buildPriceShape(shape)
    shape.setError('error' in built ? built.error : null)
    if ('error' in built) return
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
          <DialogFooter>
            {/* The rate card's cells have no remove icon; removing starts here. */}
            <Button type="button" variant="ghost" className="sm:mr-auto text-destructive hover:text-destructive" onClick={onRemove}>Remove price</Button>
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
