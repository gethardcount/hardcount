import { useEffect } from 'react'
import { useForm, useFieldArray } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'
import { useMutation, useQuery } from '@tanstack/react-query'
import { Loader2, Plus, Trash2 } from 'lucide-react'

import { api, type Customer, type Plan, type Subscription } from '@/lib/api'
import { applyApiError } from '@/lib/formErrors'
import { hasUsableCard } from '@/lib/paymentReadiness'
import { toast } from 'sonner'

import { CustomerCombobox } from '@/components/CustomerCombobox'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Checkbox } from '@/components/ui/checkbox'
import { Separator } from '@/components/ui/separator'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import {
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'

// Same schema as the standalone Subscriptions-page form had — single
// source of truth now that CustomerDetail's reduced 4-field variant
// has been folded in. Industry convention (Stripe / Chargebee) is one
// create-subscription form across entry points, with the parent ID
// pre-filled.
const schema = z.object({
  code: z.string().min(1, 'Code is required').regex(/^[a-zA-Z0-9_-]+$/, 'Only letters, numbers, hyphens, and underscores'),
  display_name: z.string().min(1, 'Display name is required'),
  customer_id: z.string().min(1, 'Customer is required'),
  items: z.array(z.object({
    plan_id: z.string().min(1, 'Plan is required'),
    quantity: z.string().refine(v => v === '' || (Number.isInteger(Number(v)) && Number(v) >= 1), 'Quantity must be a positive integer'),
  })).min(1, 'At least one item is required').refine(
    (items) => new Set(items.map(i => i.plan_id)).size === items.length,
    { message: 'Each plan can only appear once per subscription' },
  ),
  start_now: z.boolean(),
  billing_time: z.string(),
  trial_days: z.string(),
  usage_cap_units: z.string(),
  overage_action: z.string(),
})

type FormData = z.infer<typeof schema>

function emptyDefaults(customerId: string): FormData {
  return {
    code: '',
    display_name: '',
    customer_id: customerId,
    items: [{ plan_id: '', quantity: '1' }],
    start_now: true,
    billing_time: 'calendar',
    trial_days: '',
    usage_cap_units: '',
    overage_action: 'charge',
  }
}

interface Props {
  open: boolean
  onOpenChange: (open: boolean) => void
  plans: Plan[]
  // When set, the customer picker is hidden and customer_id is locked
  // to this customer (CustomerDetail entry point). When unset, the
  // dialog renders its own server-searched picker.
  lockedCustomer?: Customer
  // Clock id → display name for the ADR-027 inherit hint. Both entry
  // points compute this; passing nothing just hides the hint.
  clockNameMap?: Record<string, string>
  onCreated: (sub: Subscription) => void
}

export function CreateSubscriptionDialog({
  open,
  onOpenChange,
  plans,
  lockedCustomer,
  clockNameMap = {},
  onCreated,
}: Props) {
  const form = useForm<FormData>({
    resolver: zodResolver(schema),
    defaultValues: emptyDefaults(lockedCustomer?.id ?? ''),
  })
  const itemsArray = useFieldArray({ control: form.control, name: 'items' })

  // Reset on close so the next open starts clean. Also re-sync the
  // locked customer in case the parent mounts before its customer
  // query resolves.
  useEffect(() => {
    if (!open) form.reset(emptyDefaults(lockedCustomer?.id ?? ''))
    else if (lockedCustomer) form.setValue('customer_id', lockedCustomer.id)
  }, [open, lockedCustomer, form])

  const mutation = useMutation({
    mutationFn: (data: FormData) => api.createSubscription({
      code: data.code,
      display_name: data.display_name,
      customer_id: data.customer_id,
      items: data.items.map(it => ({
        plan_id: it.plan_id,
        ...(it.quantity ? { quantity: parseInt(it.quantity) } : {}),
      })),
      start_now: data.start_now,
      billing_time: data.billing_time,
      ...(data.trial_days ? { trial_days: parseInt(data.trial_days) } : {}),
      ...(data.usage_cap_units ? { usage_cap_units: parseInt(data.usage_cap_units) } : {}),
      ...(data.overage_action !== 'charge' ? { overage_action: data.overage_action } : {}),
    }),
    onSuccess: (created, vars) => {
      onCreated(created)
      // Shift-left note (2026-07-22 audit, P1-5): a just-started
      // subscription with no card on file will try to auto-charge a
      // card that isn't there. Non-blocking — the sub IS created (we
      // never gate creation on a card, matching every reference
      // platform). Gated on start_now: a scheduled sub isn't billing
      // yet. A started trial still gets the nudge — the card is due
      // before trial end, and the customer/subscription pages warn on
      // trialing subs for the same reason.
      if (vars.start_now) {
        void api.listCustomerPaymentMethods(created.customer_id)
          .then(res => {
            if (!hasUsableCard(res.data ?? [])) {
              toast.warning('No payment method on file', {
                description: "This customer's first invoice can't be collected automatically. Add a card to turn on automatic billing.",
              })
            }
          })
          .catch(() => { /* best-effort nudge — never block on the check */ })
      }
    },
    onError: (err) => {
      applyApiError(form, err, ['code', 'display_name', 'customer_id', 'items', 'billing_time', 'trial_days', 'usage_cap_units', 'overage_action'])
    },
  })

  const onSubmit = form.handleSubmit((data) => mutation.mutate(data))

  const watchedCustomerId = form.watch('customer_id')
  // The clock-inherit hint needs the selected customer's test_clock_id.
  // With the server-searched picker there is no local customers list, so
  // fetch the picked customer directly (cheap ids= point read).
  const { data: pickedCustomerData } = useQuery({
    queryKey: ['create-sub-picked-customer', watchedCustomerId],
    queryFn: () => api.listCustomers(`ids=${watchedCustomerId}&limit=1`),
    enabled: !lockedCustomer && !!watchedCustomerId,
    staleTime: 30_000,
  })
  const inheritCustomer = lockedCustomer ?? pickedCustomerData?.data?.[0] ?? null
  const inheritedClockName = inheritCustomer?.test_clock_id
    ? (clockNameMap[inheritCustomer.test_clock_id] || inheritCustomer.test_clock_id)
    : ''

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Create subscription</DialogTitle>
          <DialogDescription>
            Add a new subscription to start billing a customer.
          </DialogDescription>
        </DialogHeader>
        <Form {...form}>
          <form onSubmit={onSubmit} noValidate className="space-y-5">
            {/* Basic info */}
            <div className="grid grid-cols-2 gap-4">
              <FormField
                control={form.control}
                name="display_name"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Display name</FormLabel>
                    <FormControl>
                      <Input placeholder="Acme Pro Monthly" maxLength={255} {...field} />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
              <FormField
                control={form.control}
                name="code"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Code</FormLabel>
                    <FormControl>
                      <Input placeholder="acme-pro" maxLength={100} className="font-mono" {...field} />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
            </div>

            {!lockedCustomer && (
              <FormField
                control={form.control}
                name="customer_id"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Customer</FormLabel>
                    <FormControl>
                      {/* Server-searched picker (P11): the old native select
                          rendered from the page's display-ref customer list —
                          customers with no existing subscription (or past the
                          50-row page) were absent, so their FIRST subscription
                          was uncreatable from the dashboard. */}
                      <CustomerCombobox
                        value={field.value}
                        onChange={field.onChange}
                        placeholder="Search customers..."
                      />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
            )}

            {inheritedClockName && (
              <div className="rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-xs text-amber-900">
                This subscription will inherit the customer's test clock — <span className="font-medium">{inheritedClockName}</span>.
              </div>
            )}

            {/* Items — dynamic array. Backend rejects duplicate plan_ids
                so we mirror that in zod. */}
            <div className="space-y-2">
              <div className="flex items-center justify-between">
                <FormLabel>Plans</FormLabel>
                <Button
                  type="button"
                  size="sm"
                  variant="outline"
                  onClick={() => itemsArray.append({ plan_id: '', quantity: '1' })}
                >
                  <Plus size={14} className="mr-1.5" />
                  Add item
                </Button>
              </div>
              <div className="space-y-2">
                {itemsArray.fields.map((field, idx) => (
                  <div key={field.id} className="flex items-start gap-2">
                    <FormField
                      control={form.control}
                      name={`items.${idx}.plan_id`}
                      render={({ field }) => (
                        <FormItem className="flex-1">
                          <FormControl>
                            <select
                              aria-label="Plan"
                              value={field.value}
                              onChange={field.onChange}
                              className="flex h-9 w-full rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-xs transition-colors focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
                            >
                              <option value="">Select plan...</option>
                              {plans.map(p => (
                                <option key={p.id} value={p.id}>{p.name}</option>
                              ))}
                            </select>
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name={`items.${idx}.quantity`}
                      render={({ field }) => (
                        <FormItem className="w-24">
                          <FormControl>
                            <Input type="number" min={1} placeholder="Qty" {...field} />
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                    {itemsArray.fields.length <= 1 ? (
                      <Tooltip>
                        <TooltipTrigger render={<span className="inline-block cursor-not-allowed" />}>
                          <Button
                            type="button"
                            size="sm"
                            variant="ghost"
                            className="h-9 px-2 text-muted-foreground hover:text-destructive"
                            disabled
                          >
                            <Trash2 size={14} />
                          </Button>
                        </TooltipTrigger>
                        <TooltipContent>A subscription requires at least one item.</TooltipContent>
                      </Tooltip>
                    ) : (
                      <Tooltip>
                        <TooltipTrigger
                          render={
                            <Button
                              type="button"
                              size="sm"
                              variant="ghost"
                              className="h-9 px-2 text-muted-foreground hover:text-destructive"
                              onClick={() => itemsArray.remove(idx)}
                            />
                          }
                        >
                          <Trash2 size={14} />
                        </TooltipTrigger>
                        <TooltipContent>Remove</TooltipContent>
                      </Tooltip>
                    )}
                  </div>
                ))}
              </div>
              {form.formState.errors.items?.root && (
                <p className="text-xs text-destructive">{form.formState.errors.items.root.message}</p>
              )}
              {form.formState.errors.items?.message && (
                <p className="text-xs text-destructive">{form.formState.errors.items.message}</p>
              )}
            </div>

            {/* Billing config */}
            <Separator />
            <div className="grid grid-cols-2 gap-4">
              <FormField
                control={form.control}
                name="billing_time"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Billing cycle</FormLabel>
                    <FormControl>
                      <select
                        value={field.value}
                        onChange={field.onChange}
                        className="flex h-9 w-full rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-xs transition-colors focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
                      >
                        <option value="calendar">Calendar (month start)</option>
                        <option value="anniversary">Anniversary (sub start)</option>
                      </select>
                    </FormControl>
                  </FormItem>
                )}
              />
              <FormField
                control={form.control}
                name="trial_days"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Trial period</FormLabel>
                    <FormControl>
                      <Input type="number" min={0} placeholder="0 days" {...field} />
                    </FormControl>
                  </FormItem>
                )}
              />
            </div>

            <FormField
              control={form.control}
              name="start_now"
              render={({ field }) => (
                <FormItem className="flex flex-row items-center gap-2 rounded-md border border-input px-3 py-2.5">
                  <FormControl>
                    <Checkbox
                      checked={field.value}
                      onCheckedChange={field.onChange}
                    />
                  </FormControl>
                  <div>
                    <FormLabel className="text-sm font-medium">Start immediately</FormLabel>
                    <p className="text-xs text-muted-foreground">Activate and set the first billing period now</p>
                  </div>
                </FormItem>
              )}
            />

            {/* Usage limits */}
            <Separator />
            <div className="grid grid-cols-2 gap-4">
              <FormField
                control={form.control}
                name="usage_cap_units"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Usage cap</FormLabel>
                    <FormControl>
                      <Input type="number" min={0} placeholder="Unlimited" {...field} />
                    </FormControl>
                    <FormDescription>Max units per period</FormDescription>
                  </FormItem>
                )}
              />
              <FormField
                control={form.control}
                name="overage_action"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Over-limit Action</FormLabel>
                    <FormControl>
                      <select
                        value={field.value}
                        onChange={field.onChange}
                        className="flex h-9 w-full rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-xs transition-colors focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
                      >
                        <option value="charge">Charge overage</option>
                        <option value="block">Cap at limit</option>
                      </select>
                    </FormControl>
                  </FormItem>
                )}
              />
            </div>

            <DialogFooter>
              <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
                Cancel
              </Button>
              <Button type="submit" disabled={mutation.isPending}>
                {mutation.isPending ? (
                  <>
                    <Loader2 size={14} className="animate-spin mr-2" />
                    Creating...
                  </>
                ) : (
                  'Create subscription'
                )}
              </Button>
            </DialogFooter>
          </form>
        </Form>
      </DialogContent>
    </Dialog>
  )
}
