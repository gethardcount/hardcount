import { useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { Loader2 } from 'lucide-react'

import { api, ApiError, formatCents, getCurrencySymbol } from '@/lib/api'
import { authApi } from '@/lib/auth'
import { useAuth } from '@/contexts/AuthContext'
import { CURRENCIES as GEO_CURRENCIES } from '@/lib/geo'
import { invoiceCurrencyPin } from '@/lib/customerCurrency'
import { stripeKeyState } from '@/lib/stripeVerify'
import { invalidateMoneySurfaces } from '@/lib/invalidateMoney'
import { NO_ANSWER_TITLE, outcomeUnknown } from '@/lib/errorCopy'
import { confirmAfterCreate, creditValidityText, discountLine, payOptions, sellCreditAmounts, type PayChoice } from '@/lib/sellCredit'
import { cn } from '@/lib/utils'
import { useCustomerCurrency } from '@/hooks/useCustomerCurrency'
import { useTenantSettings } from '@/hooks/useTenantSettings'
import { useSingleFlight } from '@/hooks/useSingleFlight'
import { CustomerCombobox } from '@/components/CustomerCombobox'
import { Callout } from '@/components/Callout'
import { LoadError } from '@/components/LoadError'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'

// What the dialog says when the create's answer is lost. One Idempotency-Key
// per opening means creating again returns the top-up the first try made.
const SELL_CREDIT_NO_ANSWER =
  "The server didn't answer, so the top-up may have been made. Create it again without changes: that can't make a second top-up."

type FieldErrors = { customer?: string; currency?: string; pay?: string; credit?: string; choice?: string; form?: string; noAnswer?: boolean }

/**
 * Sell credit: the customer pays for credit up front. It creates a top-up
 * (an invoice with one commit line, POST /v1/invoices) and lands on its page,
 * with the confirm for the chosen way to pay already open, so that confirm is
 * where money or an email moves. customerId '' shows a customer picker.
 */
export function SellCreditDialog({ customerId, open, onOpenChange }: {
  customerId: string
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const { user } = useAuth()
  const [picked, setPicked] = useState('')
  const cid = customerId || picked
  const [pickedCurrency, setPickedCurrency] = useState<string>()
  const [payText, setPayText] = useState('')
  const [creditText, setCreditText] = useState('')
  const [choice, setChoice] = useState<PayChoice | null>(null)
  const [errors, setErrors] = useState<FieldErrors>({})
  const [submitting, setSubmitting] = useState(false)
  // One Idempotency-Key per opening; the dialog unmounts on close.
  const [idemKey] = useState(() => crypto.randomUUID())

  const customer = useQuery({ queryKey: ['customer', cid], queryFn: () => api.getCustomer(cid), enabled: !!cid })
  // The currency, as New invoice decides it: the customer's, fixed when the
  // billing profile or a live plan sets it (the server refuses another).
  const currencyRead = useCustomerCurrency(cid || undefined)
  const pin = currencyRead.currency ? invoiceCurrencyPin(currencyRead.currency, 'the top-up') : null
  const currency = pin ? currencyRead.currency?.currency : (pickedCurrency ?? currencyRead.currency?.currency)
  const settings = useTenantSettings()

  // The facts the ways to pay need: the same checks the server's send makes.
  const whoami = useQuery({ queryKey: ['whoami'], queryFn: () => authApi.whoami(), staleTime: Infinity })
  const emailSettings = useQuery({ queryKey: ['email-settings'], queryFn: () => api.getEmailSettings() })
  const stripeCreds = useQuery({ queryKey: ['stripe-credentials'], queryFn: () => api.listStripeCredentials(), enabled: !!user })
  const cards = useQuery({
    queryKey: ['customer-payment-methods', cid],
    queryFn: () => api.listCustomerPaymentMethods(cid),
    enabled: !!cid,
  })
  const options = payOptions({
    noCustomer: !cid,
    customerEmail: customer.data ? (customer.data.email ?? '') : undefined,
    smtpConfigured: whoami.data?.smtp_configured,
    paymentPageSet: emailSettings.data?.links.find(l => l.env === 'HOSTED_INVOICE_BASE_URL')?.set,
    stripeConnected: stripeCreds.data && user ? stripeKeyState(stripeCreds.data.data, user.livemode) === 'connected' : undefined,
    livemode: user?.livemode ?? false,
    card: cards.data ? (cards.data.data.find(pm => pm.is_default) ?? null) : undefined,
    linkFactsFailed: whoami.isError || emailSettings.isError || stripeCreds.isError,
    cardFactsFailed: cards.isError || stripeCreds.isError,
  })

  const amounts = sellCreditAmounts(payText, creditText)
  const money = (cents: number) => (currency ? formatCents(cents, currency) : '')
  const differs = amounts.ok && currency ? discountLine(amounts.payCents, amounts.creditCents, money) : null
  const symbol = currency ? ` (${getCurrencySymbol(currency).trim() || currency})` : ''

  const submit = useSingleFlight(async () => {
    const next: FieldErrors = {}
    if (!cid) next.customer = 'Select a customer'
    if (!currency) next.currency = 'Select a currency'
    if (!amounts.ok) Object.assign(next, amounts.errors)
    const picked = options.find(o => o.value === choice)
    if (!picked || picked.state !== 'available') next.choice = 'Choose how the customer will pay'
    setErrors(next)
    if (Object.keys(next).length > 0 || !amounts.ok || !currency || !choice) return
    setSubmitting(true)
    try {
      const lineItems = [{
        description: 'Prepaid credit', line_type: 'add_on', quantity: 1,
        unit_amount_cents: amounts.payCents, commit_granted_cents: amounts.creditCents,
      }]
      const topup = await api.createInvoice({ customer_id: cid, currency, line_items: lineItems }, idemKey)
      invalidateMoneySurfaces(queryClient, { customerId: cid, invoiceId: topup.id })
      onOpenChange(false)
      toast.success('Top-up created')
      const confirm = confirmAfterCreate(choice)
      navigate(`/invoices/${topup.id}${confirm ? `?next=${confirm}` : ''}`)
    } catch (err) {
      // A lost answer may still have made the top-up: read the lists again.
      invalidateMoneySurfaces(queryClient, { customerId: cid })
      const field = err instanceof ApiError ? err.field ?? '' : ''
      if (outcomeUnknown(err)) setErrors({ form: SELL_CREDIT_NO_ANSWER, noAnswer: true })
      else if (field.endsWith('unit_amount_cents')) setErrors({ pay: (err as Error).message })
      else if (field.endsWith('commit_granted_cents')) setErrors({ credit: (err as Error).message })
      else setErrors({ form: err instanceof Error ? err.message : "Couldn't create the top-up" })
    } finally {
      setSubmitting(false)
    }
  })

  return (
    <Dialog open={open} onOpenChange={(o) => { if (!submitting) onOpenChange(o) }}>
      {/* eslint-disable-next-line no-restricted-syntax -- DialogContent scrolls as ONE box including its own close button, as New invoice does; a scrolling body inside a fixed shell is a redesign of every dialog. */}
      <DialogContent className="sm:max-w-md max-h-[90vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>Sell credit</DialogTitle>
        </DialogHeader>
        <form onSubmit={(e) => { e.preventDefault(); void submit() }} noValidate className="space-y-4">
          <p className="text-sm text-muted-foreground">
            The customer pays up front, and the credit is added to their balance when the payment arrives.
          </p>

          {customerId ? (
            <div className="bg-muted rounded-lg px-4 py-3">
              <p className="text-sm text-muted-foreground">Customer</p>
              <p className="text-sm font-medium text-foreground mt-0.5">{customer.data?.display_name ?? '…'}</p>
            </div>
          ) : (
            <div>
              <Label className="text-sm font-medium">Customer</Label>
              <div className="mt-2">
                <CustomerCombobox
                  value={picked}
                  onChange={(v) => { setPicked(v); setPickedCurrency(undefined); setChoice(null); setErrors({}) }}
                  placeholder="Search customers..."
                />
              </div>
              {errors.customer && <p className="text-destructive text-sm mt-1">{errors.customer}</p>}
            </div>
          )}

          <div className="space-y-2">
            <Label htmlFor="sell-credit-currency">Currency</Label>
            <Select
              items={GEO_CURRENCIES.map(c => ({ value: c.code, label: `${c.symbol} ${c.code} — ${c.label}` }))}
              value={currency ?? null}
              onValueChange={(v) => { if (v) { setPickedCurrency(v); setErrors(e => ({ ...e, currency: undefined })) } }}
              disabled={pin !== null || !cid}
            >
              <SelectTrigger id="sell-credit-currency" className="w-full">
                <SelectValue placeholder="Select currency..." />
              </SelectTrigger>
              <SelectContent>
                {GEO_CURRENCIES.map(c => (
                  <SelectItem key={c.code} value={c.code}>{c.symbol} {c.code} — {c.label}</SelectItem>
                ))}
              </SelectContent>
            </Select>
            {pin && <p className="text-xs text-muted-foreground">{pin}</p>}
            {cid && !currency && (currencyRead.failure ? (
              <LoadError what="this customer's currency" error={currencyRead.failure.error} onRetry={currencyRead.failure.retry} className="py-3">
                You can also pick a currency above.
              </LoadError>
            ) : (
              <p className="text-sm text-muted-foreground">Loading this customer's currency…</p>
            ))}
            {errors.currency && <p className="text-destructive text-sm">{errors.currency}</p>}
          </div>

          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <div className="space-y-2">
              <Label htmlFor="sell-credit-pay">What the customer pays{symbol}</Label>
              <Input id="sell-credit-pay" type="text" inputMode="decimal" placeholder="100.00" value={payText}
                aria-invalid={!!errors.pay}
                onChange={e => { setPayText(e.target.value); setErrors(er => ({ ...er, pay: undefined })) }} />
              {errors.pay && <p className="text-xs text-destructive">{errors.pay}</p>}
            </div>
            <div className="space-y-2">
              <Label htmlFor="sell-credit-credit">The credit they get{symbol}</Label>
              <Input id="sell-credit-credit" type="text" inputMode="decimal" placeholder={payText.trim() || '100.00'} value={creditText}
                aria-invalid={!!errors.credit}
                onChange={e => { setCreditText(e.target.value); setErrors(er => ({ ...er, credit: undefined })) }} />
              {errors.credit
                ? <p className="text-xs text-destructive">{errors.credit}</p>
                : <p className="text-xs text-muted-foreground">Empty means the same as they pay.</p>}
            </div>
          </div>
          {differs && <p className="-mt-2 text-xs text-muted-foreground">{differs}</p>}

          <div className="space-y-1">
            <p className="text-sm font-medium text-foreground">How long the credit lasts</p>
            {settings.isError ? (
              <LoadError what="the account's settings" error={settings.error} onRetry={() => { void settings.refetch() }} className="py-2" />
            ) : (
              <p className="text-sm text-muted-foreground">
                {settings.data ? creditValidityText(settings.data.topup_credit_valid_days) : 'Loading…'}{' '}
                <Link to="/settings?tab=invoicing" className="text-primary hover:underline">Change in Settings</Link>
              </p>
            )}
          </div>

          <div>
            <Label id="sell-credit-pay-choice" className="mb-2 block">How will the customer pay?</Label>
            <div role="radiogroup" aria-labelledby="sell-credit-pay-choice" className="grid gap-2">
              {options.map(o => {
                const selected = choice === o.value
                const offered = o.state === 'available'
                return (
                  // The card holds the radio and, when what is missing is set
                  // up in Settings, a link there: never a link inside the radio.
                  <div
                    key={o.value}
                    className={cn(
                      'rounded-xl border p-3 transition-colors',
                      selected ? 'border-primary bg-primary/5' : 'border-border',
                      offered && !selected && 'hover:border-foreground/30',
                    )}
                  >
                    <button
                      type="button"
                      role="radio"
                      aria-checked={selected}
                      aria-disabled={!offered}
                      onClick={() => { if (offered) { setChoice(o.value); setErrors(e => ({ ...e, choice: undefined })) } }}
                      className={cn('flex w-full items-start gap-3 text-left', !offered && 'cursor-not-allowed opacity-60')}
                    >
                      <span aria-hidden className={cn('mt-0.5 h-4 w-4 shrink-0 rounded-full border', selected ? 'border-[5px] border-primary' : 'border-muted-foreground/50')} />
                      <span className="min-w-0">
                        <span className="block text-sm font-medium text-foreground">{o.label}</span>
                        {!o.href && <span className="mt-0.5 block text-xs text-muted-foreground">{o.note}</span>}
                      </span>
                    </button>
                    {o.href && (
                      <Link to={o.href} className="ml-7 mt-0.5 block text-xs text-primary hover:underline">{o.note}</Link>
                    )}
                  </div>
                )
              })}
            </div>
            {errors.choice && <p className="text-destructive text-sm mt-1">{errors.choice}</p>}
          </div>

          {errors.form && (errors.noAnswer ? (
            <Callout tone="amber" title={NO_ANSWER_TITLE} role="status">{errors.form}</Callout>
          ) : (
            <div className="rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 text-sm text-destructive">{errors.form}</div>
          ))}

          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)} disabled={submitting}>Cancel</Button>
            <Button type="submit" disabled={submitting}>
              {submitting ? <><Loader2 size={14} className="animate-spin mr-2" />Creating…</> : 'Create top-up'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
