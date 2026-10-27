import { useState } from 'react'
import { Trash2 } from 'lucide-react'
import { getCurrencySymbol, type PriceShape } from '@/lib/api'
import { dollarsToRateCents } from '@/lib/priceInput'
import { PRICING_MODES, pricingModeLabel, shiftDecimal } from '@/lib/priceDisplay'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from '@/components/ui/select'

// The price-shape form, shared by a plan's prices and a customer's price
// overrides: a mode and the fields that mode uses. Rates are typed in
// currency units and sent as decimal cents (dollarsToRateCents), so sub-cent
// rates stay exact.

const MODE_OPTIONS = PRICING_MODES.map(value => ({ value, label: pricingModeLabel(value) }))

const MODE_HELP: Record<string, string> = {
  per_unit: 'Fixed price per unit',
  graduated: 'Each tier prices the units above the tier before it',
  package: 'Charge per block of units. A part block is charged as a whole one.',
}

interface TierRow { upTo: string; price: string }

export interface PriceShapeState {
  mode: string
  setMode: (m: string) => void
  unitAmount: string
  setUnitAmount: (v: string) => void
  tiers: TierRow[]
  setTiers: React.Dispatch<React.SetStateAction<TierRow[]>>
  packageSize: string
  setPackageSize: (v: string) => void
  packageAmount: string
  setPackageAmount: (v: string) => void
}

// usePriceShape holds the form's state, seeded from an existing shape when
// one is given (rates shown in currency units, exactly).
export function usePriceShape(prefill?: PriceShape): PriceShapeState {
  const [mode, setMode] = useState(prefill?.mode ?? 'per_unit')
  const [unitAmount, setUnitAmount] = useState(prefill?.unit_amount_cents ? shiftDecimal(prefill.unit_amount_cents, -2) : '')
  const [tiers, setTiers] = useState<TierRow[]>(
    prefill?.tiers?.length
      ? prefill.tiers.map(t => ({ upTo: t.up_to != null ? String(t.up_to) : '', price: shiftDecimal(t.unit_amount_cents || '0', -2) }))
      : [{ upTo: '1000', price: '0.10' }, { upTo: '', price: '0.05' }],
  )
  const [packageSize, setPackageSize] = useState(prefill?.package_size ? String(prefill.package_size) : '100')
  const [packageAmount, setPackageAmount] = useState(prefill?.package_amount_cents ? shiftDecimal(prefill.package_amount_cents, -2) : '10.00')
  return { mode, setMode, unitAmount, setUnitAmount, tiers, setTiers, packageSize, setPackageSize, packageAmount, setPackageAmount }
}

// buildPriceShape turns the form into the request's shape, or an error to
// show. A blank or unreadable rate is refused here: the server accepts 0 as a
// deliberate free rate, so it cannot catch a rate left empty.
export function buildPriceShape(f: PriceShapeState): { shape: PriceShape } | { error: string } {
  if (f.mode === 'per_unit') {
    const cents = dollarsToRateCents(f.unitAmount)
    if (cents === null) return { error: 'Price per unit: enter a price as a plain number, for example 0.01. Use 0 for free.' }
    return { shape: { mode: 'per_unit', unit_amount_cents: cents } }
  }
  if (f.mode === 'graduated') {
    const { tiers } = f
    for (let i = 0; i < tiers.length - 1; i++) {
      const current = parseInt(tiers[i].upTo) || 0
      const next = i + 1 === tiers.length - 1 ? Infinity : (parseInt(tiers[i + 1].upTo) || 0)
      if (current <= 0) return { error: `Tier ${i + 1}: "Up to" must be greater than 0` }
      if (next !== Infinity && next <= current) return { error: `Tier ${i + 2}: "Up to" must be greater than ${current}` }
    }
    const out: { up_to: number | null; unit_amount_cents: string }[] = []
    for (let i = 0; i < tiers.length; i++) {
      const cents = dollarsToRateCents(tiers[i].price)
      if (cents === null) return { error: `Tier ${i + 1}: enter a price as a plain number, for example 0.01. Use 0 for free.` }
      // The last tier has no upper bound: up_to null.
      out.push({ up_to: i === tiers.length - 1 ? null : parseInt(tiers[i].upTo) || 0, unit_amount_cents: cents })
    }
    return { shape: { mode: 'graduated', tiers: out } }
  }
  const cents = dollarsToRateCents(f.packageAmount)
  if (cents === null) return { error: 'Price per package: enter a price as a plain number, for example 10.00. Use 0 for free.' }
  return { shape: { mode: 'package', package_size: parseInt(f.packageSize) || 1, package_amount_cents: cents } }
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
          <Input id={`${idPrefix}-unit-price`} type="number" step="any" min={0} value={f.unitAmount}
            onChange={e => f.setUnitAmount(e.target.value)} placeholder="0.01" className="mt-1" />
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
              <div key={idx} className="grid grid-cols-[1fr_1fr_36px] gap-0 px-3 py-2 border-b border-border last:border-b-0 items-center">
                <div className="pr-2">
                  {/* The last tier has no upper limit, so its box takes no number. */}
                  <Input type="number" min={0} value={idx === f.tiers.length - 1 ? '' : tier.upTo}
                    aria-label={`Tier ${idx + 1} up to`}
                    disabled={idx === f.tiers.length - 1}
                    onChange={e => f.setTiers(t => t.map((r, i) => i === idx ? { ...r, upTo: e.target.value } : r))}
                    placeholder={idx === f.tiers.length - 1 ? 'No limit' : '1000'} />
                </div>
                <div className="pr-2">
                  <Input type="number" step="any" min={0} value={tier.price}
                    aria-label={`Tier ${idx + 1} price`}
                    onChange={e => f.setTiers(t => t.map((r, i) => i === idx ? { ...r, price: e.target.value } : r))}
                    placeholder="0.01" />
                </div>
                <div className="flex justify-center">
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
            <Input id={`${idPrefix}-pkg-size`} type="number" min={1} value={f.packageSize}
              onChange={e => f.setPackageSize(e.target.value)} placeholder="100" className="mt-1" />
          </div>
          <div>
            <Label htmlFor={`${idPrefix}-pkg-price`}>Price per package ({sym})</Label>
            <Input id={`${idPrefix}-pkg-price`} type="number" step="any" min={0} value={f.packageAmount}
              onChange={e => f.setPackageAmount(e.target.value)} placeholder="10.00" className="mt-1" />
          </div>
        </div>
      )}
    </>
  )
}
