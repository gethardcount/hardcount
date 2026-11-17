import { useState } from 'react'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'
import { Loader2 } from 'lucide-react'
import { useAuth } from '@/contexts/AuthContext'
import { useTenantTimezone } from '@/hooks/useTenantTimezone'
import { api } from '@/lib/api'
import { addDaysInTZ, civilDateFrom, endOfDayInTZ, formatExpiryHintInTZ } from '@/lib/dates'
import { wallClockNow } from '@/lib/effectiveNow'
import { applyApiError } from '@/lib/formErrors'
import type { APIKeyKeyType } from '@/lib/gen/schemas/aPIKeyKeyType'
import { cn } from '@/lib/utils'
import { KEY_TYPES } from '@/components/apikeys/keyTypes'
import { PresetButtons } from '@/components/apikeys/PresetButtons'
import { Button } from '@/components/ui/button'
import { DatePicker } from '@/components/ui/date-picker'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Form, FormControl, FormDescription, FormField, FormItem, FormLabel, FormMessage } from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'

// A name is 1 to 100 characters once trimmed, counted as the server counts
// them: by code point, so an emoji is one character.
const MAX_NAME_CHARS = 100

const schema = z.object({
  name: z.string().trim().min(1, 'Name is required')
    .refine(s => [...s].length <= MAX_NAME_CHARS, `Name must be at most ${MAX_NAME_CHARS} characters`),
})

// A number is how many days the key lives.
const EXPIRY_PRESETS = [
  { value: 'never', label: 'No expiration' },
  { value: 30, label: '30 days' },
  { value: 90, label: '90 days' },
  { value: 365, label: '1 year' },
  { value: 'custom', label: 'Custom' },
] as const

/** Asks for a new key's name, type and expiry, creates it, and gives onCreated the full key. */
export function CreateKeyDialog({ onClose, onCreated }: { onClose: () => void; onCreated: (rawKey: string) => void }) {
  const [keyType, setKeyType] = useState<APIKeyKeyType>('secret')
  const [expiry, setExpiry] = useState<(typeof EXPIRY_PRESETS)[number]['value']>('never')
  const [customDate, setCustomDate] = useState('')
  const [customDateError, setCustomDateError] = useState('')
  const tenantTZ = useTenantTimezone()
  // The server gives a new key the mode of the dashboard session.
  const { user } = useAuth()
  const form = useForm<z.infer<typeof schema>>({ resolver: zodResolver(schema), defaultValues: { name: '' } })

  const now = wallClockNow()
  const expiresAt = typeof expiry === 'number' ? addDaysInTZ(expiry, tenantTZ)
    : expiry === 'custom' && customDate ? endOfDayInTZ(customDate, tenantTZ)
    : undefined

  const onSubmit = form.handleSubmit(async ({ name }) => {
    setCustomDateError('')
    if (expiry === 'custom' && !customDate) {
      setCustomDateError('Please select an expiration date')
      return
    }
    try {
      const res = await api.createApiKey({ name, key_type: keyType, expires_at: expiresAt })
      onCreated(res.raw_key)
    } catch (err) {
      applyApiError(form, err, ['name', 'key_type', 'expires_at'], { toastTitle: 'Failed to create API key' })
    }
  })

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Create API key</DialogTitle>
          {user && <DialogDescription>{`This key will work in ${user.livemode ? 'live' : 'test'} mode only.`}</DialogDescription>}
        </DialogHeader>
        <Form {...form}>
          <form onSubmit={onSubmit} noValidate className="space-y-4">
            <FormField
              control={form.control}
              name="name"
              render={({ field }) => (
                <FormItem>
                  <FormLabel>Name</FormLabel>
                  <FormControl>
                    <Input placeholder="e.g. Production, Staging, CI/CD" {...field} />
                  </FormControl>
                  <FormDescription>A descriptive name to identify this key</FormDescription>
                  <FormMessage />
                </FormItem>
              )}
            />

            <div>
              <Label id="create-key-type" className="mb-2 block">Key type</Label>
              <div role="radiogroup" aria-labelledby="create-key-type" className="grid gap-2">
                {KEY_TYPES.map(({ value, label, hint, icon: Icon }) => {
                  const selected = keyType === value
                  return (
                    <button
                      key={value}
                      type="button"
                      role="radio"
                      aria-checked={selected}
                      onClick={() => setKeyType(value)}
                      className={cn(
                        'flex items-start gap-3 p-3 rounded-xl border text-left transition-colors',
                        selected ? 'border-primary bg-primary/5' : 'border-border hover:border-foreground/30'
                      )}
                    >
                      <Icon size={18} className={cn('mt-0.5 shrink-0', selected ? 'text-primary' : 'text-muted-foreground')} />
                      <span>
                        <span className="block text-sm font-medium text-foreground">{label}</span>
                        <span className="block text-xs text-muted-foreground mt-0.5">{hint}</span>
                      </span>
                    </button>
                  )
                })}
              </div>
            </div>

            <div>
              <Label className="mb-2 block">Expiration</Label>
              <PresetButtons presets={EXPIRY_PRESETS} value={expiry} onChange={setExpiry} />
              {expiry === 'custom' && (
                <div className="mt-3">
                  {/* The earliest pick is tomorrow in the account's zone: a key
                      that expires later today would stop its integrations
                      before anyone notices. */}
                  <DatePicker
                    value={customDate}
                    onChange={(d) => { setCustomDate(d); setCustomDateError('') }}
                    placeholder="Select expiration date"
                    className="w-56"
                    timeZone={tenantTZ}
                    now={now}
                    min={civilDateFrom(now, 1, tenantTZ)}
                  />
                </div>
              )}
              {expiresAt && (
                <p className="text-xs text-muted-foreground mt-2">
                  Key will expire on {formatExpiryHintInTZ(expiresAt, tenantTZ)}
                </p>
              )}
              {customDateError && <p className="text-destructive text-sm mt-2">{customDateError}</p>}
            </div>

            <DialogFooter>
              <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
              <Button type="submit" disabled={form.formState.isSubmitting}>
                {form.formState.isSubmitting ? <><Loader2 size={14} className="animate-spin mr-2" /> Creating…</> : 'Create key'}
              </Button>
            </DialogFooter>
          </form>
        </Form>
      </DialogContent>
    </Dialog>
  )
}
