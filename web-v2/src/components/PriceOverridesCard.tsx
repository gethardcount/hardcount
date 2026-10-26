import { useMemo, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Loader2, Trash2 } from 'lucide-react'
import { api, formatDate, type Meter, type Plan, type PlanPrice, type PriceOverride } from '@/lib/api'
import { priceRate } from '@/lib/priceDisplay'
import { showApiError } from '@/lib/formErrors'
import { PriceShapeFields, buildPriceShape, usePriceShape } from '@/components/PriceShapeFields'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { Input } from '@/components/ui/input'
import { Combobox } from '@/components/Combobox'
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from '@/components/ui/dialog'

// PriceOverridesCard is the operator home for a customer's negotiated rates:
// each one replaces the rate of one plan price for this customer (ADR-130).
// The card shows each deal next to the price's list rate (the contrast is
// the information). A deal keeps applying when the price's rate changes,
// ends when the price is removed, and every change applies from the
// customer's next billing period.

// listedPrice is one plan price with the names the card shows for it.
interface ListedPrice {
  price: PlanPrice
  plan: Plan
  meter?: Meter
}

function describe(p: ListedPrice): string {
  const labels = Object.entries(p.price.dimension_match ?? {}).map(([k, v]) => `${k}=${v}`).join(', ')
  return `${p.plan.name} · ${p.meter?.name ?? p.price.meter_id}${labels ? ` (${labels})` : ''}`
}

function listRate(p: ListedPrice): string {
  return priceRate({ ...p.price, currency: p.plan.currency }, p.meter?.unit ?? '')
}

// readOnly mirrors the archived-customer gate the rest of the customer page
// applies: an archived customer's deals can be read, not changed.
export function PriceOverridesCard({ customerId, readOnly = false }: { customerId: string; readOnly?: boolean }) {
  const queryClient = useQueryClient()
  const [creating, setCreating] = useState(false)
  const [confirmDelete, setConfirmDelete] = useState<PriceOverride | null>(null)

  const { data: overridesData, isLoading } = useQuery({
    queryKey: ['price-overrides', customerId],
    queryFn: () => api.listPriceOverrides({ customerId }),
  })
  const { data: plansData } = useQuery({ queryKey: ['plans'], queryFn: () => api.listPlans() })
  const { data: metersData } = useQuery({ queryKey: ['meters'], queryFn: () => api.listMeters() })

  const overrides = (overridesData?.data ?? []).filter(o => o.active)
  const prices = useMemo(() => {
    const out = new Map<string, ListedPrice>()
    for (const plan of plansData?.data ?? []) {
      for (const price of plan.prices ?? []) {
        out.set(price.id, { price, plan, meter: metersData?.data.find(m => m.id === price.meter_id) })
      }
    }
    return out
  }, [plansData, metersData])

  const refresh = () => {
    queryClient.invalidateQueries({ queryKey: ['price-overrides', customerId] })
    queryClient.invalidateQueries({ queryKey: ['customer-usage', customerId] })
  }

  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-3">
        <CardTitle className="text-sm">Price overrides</CardTitle>
        {!readOnly && (
          <Button size="sm" variant="outline" onClick={() => setCreating(true)}>
            Add override
          </Button>
        )}
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <div className="flex justify-center py-4"><Loader2 size={16} className="animate-spin text-muted-foreground" /></div>
        ) : overrides.length === 0 ? (
          <div className="text-center py-4">
            <p className="text-sm text-muted-foreground">No negotiated prices — this customer bills at list price.</p>
            <p className="text-xs text-muted-foreground mt-1">
              An override replaces one plan price's rate for this customer only. It stays when the price's rate changes, and ends if the price is removed.
            </p>
          </div>
        ) : (
          <div className="space-y-3">
            {overrides.map(o => {
              const list = prices.get(o.price_id)
              return (
                <div key={o.id} className="flex items-start justify-between gap-3 rounded-md border border-border px-3 py-2">
                  <div className="min-w-0">
                    <p className="text-sm font-medium truncate">{list ? describe(list) : o.price_id}</p>
                    <p className="text-sm mt-0.5">
                      <span className="font-semibold">
                        {priceRate({ ...o, currency: list?.plan.currency }, list?.meter?.unit ?? '')}
                      </span>
                      {list && <span className="text-muted-foreground"> · list price {listRate(list)}</span>}
                    </p>
                    <p className="text-xs text-muted-foreground mt-0.5">
                      {o.reason ? `${o.reason} · ` : ''}since {formatDate(o.created_at)}
                    </p>
                  </div>
                  {!readOnly && (
                    <button
                      type="button"
                      onClick={() => setConfirmDelete(o)}
                      aria-label={`End override on ${list ? describe(list) : o.price_id}`}
                      className="text-muted-foreground hover:text-destructive transition-colors shrink-0 mt-1"
                    >
                      <Trash2 size={15} />
                    </button>
                  )}
                </div>
              )
            })}
          </div>
        )}
      </CardContent>

      {creating && (
        <CreateOverrideDialog
          customerId={customerId}
          prices={[...prices.values()]}
          existing={overrides}
          onClose={() => setCreating(false)}
          onCreated={() => { setCreating(false); refresh() }}
        />
      )}
      {confirmDelete && (
        <DeleteOverrideDialog
          override={confirmDelete}
          name={prices.get(confirmDelete.price_id) ? describe(prices.get(confirmDelete.price_id)!) : confirmDelete.price_id}
          onClose={() => setConfirmDelete(null)}
          onDeleted={() => { setConfirmDelete(null); refresh() }}
        />
      )}
    </Card>
  )
}

function CreateOverrideDialog({ customerId, prices, existing, onClose, onCreated }: {
  customerId: string
  prices: ListedPrice[]
  existing: PriceOverride[]
  onClose: () => void
  onCreated: () => void
}) {
  const [priceId, setPriceId] = useState('')
  const shape = usePriceShape()
  const [reason, setReason] = useState('')
  const [formError, setFormError] = useState('')
  const [saving, setSaving] = useState(false)

  const selected = prices.find(p => p.price.id === priceId)
  const replacing = !!selected && existing.some(o => o.price_id === priceId)

  const options = useMemo(() =>
    [...prices]
      .sort((a, b) => describe(a).localeCompare(describe(b)))
      .map(p => ({
        value: p.price.id,
        label: `${describe(p)} — list ${listRate(p)}`,
        keywords: [p.plan.name, p.plan.code, p.meter?.event_name ?? ''],
      })), [prices])

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!priceId) return
    setFormError('')
    const built = buildPriceShape(shape)
    if ('error' in built) { setFormError(built.error); return }
    setSaving(true)
    try {
      await api.createPriceOverride({ customer_id: customerId, price_id: priceId, reason: reason.trim() || undefined, ...built.shape })
      onCreated()
    } catch (err) {
      showApiError(err, 'Failed to create price override')
    } finally {
      setSaving(false)
    }
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Add price override</DialogTitle>
          <DialogDescription>
            A negotiated rate for this customer on one plan price. It applies from this customer's
            next billing period; the period in progress keeps the rate it started with. If the customer
            subscribes to the plan or moves to it after you save, it applies from then, unless the same
            subscription was on the plan earlier in that billing period. On a test clock, advance the clock first.
          </DialogDescription>
        </DialogHeader>
        <form onSubmit={submit} noValidate className="space-y-4">
          <div className="space-y-2">
            <Label>Price</Label>
            <Combobox
              value={priceId}
              onChange={setPriceId}
              options={options}
              placeholder="Select a plan price..."
              emptyMessage="No plan prices yet. Add one on a plan first."
              triggerClassName="w-full"
            />
            {replacing && (
              <p className="text-xs text-amber-600 dark:text-amber-500">
                This customer already has an override on this price. Saving replaces it from the next billing period.
              </p>
            )}
          </div>

          <PriceShapeFields
            f={shape}
            currency={selected?.plan.currency ?? 'USD'}
            idPrefix="override"
            hint={selected ? `List price: ${listRate(selected)}. Sub-cent rates allowed.` : undefined}
          />

          <div>
            <Label htmlFor="override-reason">Reason (optional)</Label>
            <Input id="override-reason" value={reason} onChange={e => setReason(e.target.value)} maxLength={255}
              placeholder="e.g. Annual contract — negotiated Jan 2027" className="mt-1" />
            <p className="text-xs text-muted-foreground mt-1">Shown on this card and in the audit trail.</p>
          </div>

          {formError && <p className="text-sm text-destructive">{formError}</p>}

          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={saving || !priceId}>
              {saving ? <><Loader2 size={14} className="animate-spin mr-2" /> Saving...</> : 'Save override'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function DeleteOverrideDialog({ override, name, onClose, onDeleted }: {
  override: PriceOverride
  name: string
  onClose: () => void
  onDeleted: () => void
}) {
  const [deleting, setDeleting] = useState(false)

  const confirm = async () => {
    setDeleting(true)
    try {
      await api.deletePriceOverride(override.id)
      onDeleted()
    } catch (err) {
      showApiError(err, 'Failed to end price override')
      setDeleting(false)
    }
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>End price override</DialogTitle>
          <DialogDescription>{name} returns to list price for this customer.</DialogDescription>
        </DialogHeader>
        <p className="text-sm text-muted-foreground">
          <strong className="text-foreground">List price applies from the next billing period.</strong>{' '}
          The period already in progress still bills the negotiated rate it started with. If the customer
          subscribes to the plan or moves to it after you end the override, list price applies from then,
          unless the same subscription was on the plan earlier in that billing period.
        </p>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
          <Button type="button" variant="destructive" onClick={confirm} disabled={deleting}>
            {deleting ? <><Loader2 size={14} className="animate-spin mr-2" /> Ending...</> : 'End override'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
