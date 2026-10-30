import { useState } from 'react'
import { Trash2 } from 'lucide-react'
import { getCurrencySymbol, type PriceShape } from '@/lib/api'
import { PRICING_MODES, pricingModeLabel, shiftDecimal } from '@/lib/priceDisplay'
import type { PriceShapeInput, ShapeError, ShapeField } from '@/lib/priceShapeForm'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from '@/components/ui/select'

// The price-shape form, shared by a plan's prices and a customer's price
// overrides: a mode and the fields that mode uses. Rates are typed in
// currency units and sent as decimal cents (lib/priceShapeForm), so sub-cent
// rates stay exact. Rate boxes are text boxes, not number boxes: a number
// box hands the page "" for "9,50", so the form could not say what was wrong.

const MODE_OPTIONS = PRICING_MODES.map(value => ({ value, label: pricingModeLabel(value) }))

const MODE_HELP: Record<string, string> = {
  per_unit: 'Fixed price per unit',
  graduated: 'Each tier prices the units above the tier before it',
  package: 'Charge per block of units. A part block is charged as a whole one.',
}

interface TierRow { upTo: string; price: string }

export interface PriceShapeState extends PriceShapeInput {
  setMode: (m: string) => void
  setUnitAmount: (v: string) => void
  setTiers: React.Dispatch<React.SetStateAction<TierRow[]>>
  setPackageSize: (v: string) => void
  setPackageAmount: (v: string) => void
  // The refusal buildPriceShape gave on the last save, shown under its box
  // until the form changes.
  error: ShapeError | null
  setError: (e: ShapeError | null) => void
}

// usePriceShape holds the form's state, seeded from an existing shape when
// one is given (rates shown in currency units, exactly). A new price starts
// blank: an example in a box is saved as a price if nobody changes it, so
// examples go in placeholders only.
export function usePriceShape(prefill?: PriceShape): PriceShapeState {
  const [mode, setModeRaw] = useState(prefill?.mode ?? 'per_unit')
  const [unitAmount, setUnitAmountRaw] = useState(prefill?.unit_amount_cents ? shiftDecimal(prefill.unit_amount_cents, -2) : '')
  const [tiers, setTiersRaw] = useState<TierRow[]>(
    prefill?.tiers?.length
      ? prefill.tiers.map(t => ({ upTo: t.up_to != null ? String(t.up_to) : '', price: shiftDecimal(t.unit_amount_cents || '0', -2) }))
      : [{ upTo: '', price: '' }, { upTo: '', price: '' }],
  )
  const [packageSize, setPackageSizeRaw] = useState(prefill?.package_size ? String(prefill.package_size) : '')
  const [packageAmount, setPackageAmountRaw] = useState(prefill?.package_amount_cents ? shiftDecimal(prefill.package_amount_cents, -2) : '')
  const [error, setError] = useState<ShapeError | null>(null)
  // Any edit clears the last refusal.
  const edit = <T,>(set: (v: T) => void) => (v: T) => { setError(null); set(v) }
  return {
    mode, setMode: edit(setModeRaw),
    unitAmount, setUnitAmount: edit(setUnitAmountRaw),
    tiers, setTiers: edit(setTiersRaw),
    packageSize, setPackageSize: edit(setPackageSizeRaw),
    packageAmount, setPackageAmount: edit(setPackageAmountRaw),
    error, setError,
  }
}

// FieldError is the refusal line under one box, when the refusal is for it.
function FieldError({ f, field, tier }: { f: PriceShapeState; field: ShapeField; tier?: number }) {
  const e = f.error
  if (!e || e.field !== field || e.tier !== tier) return null
  return <p className="text-xs text-destructive mt-1">{e.message}</p>
}

function invalid(f: PriceShapeState, field: ShapeField, tier?: number): true | undefined {
  return f.error?.field === field && f.error.tier === tier ? true : undefined
}

// PriceShapeFields renders the mode select and the mode's fields. `currency`
// names the symbol the rates are typed in; `hint` adds a line under the
// per-unit rate (for example the list price an override replaces).
export function PriceShapeFields({ f, currency, hint, idPrefix = 'price' }: { f: PriceShapeState; currency: string; hint?: string; idPrefix?: string }) {
  const sym = getCurrencySymbol(currency)
  return (
    <>
      <div>
        <Label>Pricing model</Label>
        <Select items={MODE_OPTIONS} value={f.mode} onValueChange={(v) => f.setMode(v ?? 'per_unit')}>
          <SelectTrigger className="w-full mt-1" aria-label="Pricing model">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {MODE_OPTIONS.map(o => (
              <SelectItem key={o.value} value={o.value}>{o.label}</SelectItem>
            ))}
          </SelectContent>
        </Select>
        <p className="text-xs text-muted-foreground mt-1">{MODE_HELP[f.mode]}</p>
      </div>

      {f.mode === 'per_unit' && (
        <div>
          <Label htmlFor={`${idPrefix}-unit-price`}>Price per unit ({sym})</Label>
          <Input id={`${idPrefix}-unit-price`} type="text" inputMode="decimal" value={f.unitAmount}
            aria-invalid={invalid(f, 'unitAmount')}
            onChange={e => f.setUnitAmount(e.target.value)} placeholder="0.01" className="mt-1" />
          <FieldError f={f} field="unitAmount" />
          <p className="text-xs text-muted-foreground mt-1">{hint ?? `e.g. ${sym}0.01 per API call. Sub-cent rates allowed.`}</p>
        </div>
      )}

      {f.mode === 'graduated' && (
        <div className="space-y-3">
          <Label>Tiers</Label>
          <div className="rounded-lg border border-border overflow-hidden">
            <div className="grid grid-cols-[1fr_1fr_36px] gap-0 px-3 py-2 border-b border-border bg-muted">
              <span className="text-xs font-medium text-muted-foreground uppercase tracking-wider">Up to (units)</span>
              <span className="text-xs font-medium text-muted-foreground uppercase tracking-wider">Price per unit ({sym})</span>
              <span />
            </div>
            {f.tiers.map((tier, idx) => (
              <div key={idx} className="grid grid-cols-[1fr_1fr_36px] gap-0 px-3 py-2 border-b border-border last:border-b-0 items-start">
                <div className="pr-2">
                  {/* The last tier has no upper limit, so its box takes no number. */}
                  <Input type="number" min={1} step={1} value={idx === f.tiers.length - 1 ? '' : tier.upTo}
                    aria-label={`Tier ${idx + 1} up to`}
                    aria-invalid={invalid(f, 'tierUpTo', idx)}
                    disabled={idx === f.tiers.length - 1}
                    onChange={e => f.setTiers(t => t.map((r, i) => i === idx ? { ...r, upTo: e.target.value } : r))}
                    placeholder={idx === f.tiers.length - 1 ? 'No limit' : '1000'} />
                  <FieldError f={f} field="tierUpTo" tier={idx} />
                </div>
                <div className="pr-2">
                  <Input type="text" inputMode="decimal" value={tier.price}
                    aria-label={`Tier ${idx + 1} price`}
                    aria-invalid={invalid(f, 'tierPrice', idx)}
                    onChange={e => f.setTiers(t => t.map((r, i) => i === idx ? { ...r, price: e.target.value } : r))}
                    placeholder={idx === 0 ? '0.10' : '0.05'} />
                  <FieldError f={f} field="tierPrice" tier={idx} />
                </div>
                <div className="flex justify-center pt-2.5">
                  {f.tiers.length > 1 && (
                    <button type="button" onClick={() => f.setTiers(t => t.filter((_, i) => i !== idx))}
                      aria-label={`Remove tier ${idx + 1}`}
                      className="text-muted-foreground hover:text-destructive transition-colors">
                      <Trash2 size={14} />
                    </button>
                  )}
                </div>
              </div>
            ))}
          </div>
          <button type="button" onClick={() => f.setTiers(t => [...t, { upTo: '', price: '' }])}
            className="text-sm font-medium text-primary hover:text-primary/80 transition-colors">
            + Add tier
          </button>
          <p className="text-xs text-muted-foreground">The last tier has no upper limit.</p>
        </div>
      )}

      {f.mode === 'package' && (
        <div className="grid grid-cols-2 gap-3">
          <div>
            <Label htmlFor={`${idPrefix}-pkg-size`}>Units per package</Label>
            <Input id={`${idPrefix}-pkg-size`} type="number" min={1} step={1} value={f.packageSize}
              aria-invalid={invalid(f, 'packageSize')}
              onChange={e => f.setPackageSize(e.target.value)} placeholder="100" className="mt-1" />
            <FieldError f={f} field="packageSize" />
          </div>
          <div>
            <Label htmlFor={`${idPrefix}-pkg-price`}>Price per package ({sym})</Label>
            <Input id={`${idPrefix}-pkg-price`} type="text" inputMode="decimal" value={f.packageAmount}
              aria-invalid={invalid(f, 'packageAmount')}
              onChange={e => f.setPackageAmount(e.target.value)} placeholder="10.00" className="mt-1" />
            <FieldError f={f} field="packageAmount" />
          </div>
        </div>
      )}
    </>
  )
}
