import { useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'

import { putDunningPolicy, getGetDunningPolicyQueryKey } from '@/lib/gen/queries.gen'
import type { DunningPolicy, DunningPolicyInputFinalInvoiceAction, DunningPolicyInputFinalSubscriptionAction } from '@/lib/gen/schemas'
import { useDunningPolicy } from '@/hooks/useDunningPolicy'
import { LoadError } from '@/components/LoadError'
import { FeedSkeleton } from '@/components/ui/TableSkeleton'
import { showApiError } from '@/lib/formErrors'
import { retriesOffNotice } from '@/lib/dunningCoverage'
import { parseRetryDays, retryPlanText } from '@/lib/dunningSchedule'
import { Callout } from '@/components/Callout'

import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Label } from '@/components/ui/label'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

// The final actions (ADR-112). One list feeds both the <SelectItem>s and the
// Base UI `items` prop that lets <SelectValue> render the selected label
// (Base UI shows the raw value otherwise). Labels say what each does (pause
// is collection only, not a hard pause).
const SUBSCRIPTION_ACTION_OPTIONS = [
  { value: 'pause', label: 'Pause collection (keep drafting invoices)' },
  { value: 'cancel', label: 'Cancel the subscription' },
  { value: 'none', label: 'Leave it active' },
]

const INVOICE_ACTION_OPTIONS = [
  { value: 'none', label: 'Leave it open and due' },
  { value: 'mark_uncollectible', label: 'Write it off as bad debt' },
]

// Plain-language consequence for every combination. The old single-select
// left the debt's fate entirely unstated: an operator picking "Cancel
// subscription" had no way to learn the unpaid invoice then sits open with
// no closer anywhere in the system. Finance/ops read this, so it names
// money and next steps, not enum values.
function outcomeSentence(sub: string, inv: string): string {
  const subPart =
    sub === 'cancel' ? 'The subscription is canceled.'
    : sub === 'pause' ? 'Invoices created while collection is paused stay drafts, so nothing charges them. Retries already running on other unpaid invoices continue.'
    : 'The subscription keeps billing as normal.'
  const invPart =
    inv === 'mark_uncollectible'
      ? 'The unpaid invoice is written off as bad debt — it leaves your receivables and stays on the books for audit. If the customer pays later, record the offline payment or issue a recovery invoice; nothing charges the written-off invoice itself.'
      : 'The unpaid invoice stays open and due. Nothing closes it automatically — you collect it or write it off yourself.'
  return `${subPart} ${invPart}`
}

// What the form starts from when the mode has no policy: the policy a new
// account is created with (dunning.NewAccountPolicy), which retries three
// times and then changes nothing.
const NEW_ACCOUNT_POLICY = {
  enabled: true,
  retry_days: [3, 4, 7],
  final_subscription_action: 'none',
  final_invoice_action: 'none',
} as const

/**
 * The Payment retries page's Policy tab: one form for the account's retry
 * policy in this mode, which governs every failed payment. With no policy
 * the form starts from the policy a new account gets, and Save creates it.
 */
export function DunningPolicyTab() {
  const { data: policy, isLoading, error, refetch } = useDunningPolicy()
  return (
    <div className="mt-4 space-y-6">
      <p className="max-w-3xl text-sm text-muted-foreground">
        This policy sets how many days to wait before each retry of a failed payment, and what happens if every retry fails. It applies to every customer. Test mode and live mode each have their own. A change reaches retries already under way from their next step.
      </p>
      {isLoading && <FeedSkeleton rows={3} />}
      {!isLoading && error && <LoadError what="the retry policy" error={error} onRetry={() => refetch()} />}
      {/* Keyed on the saved policy, so the form starts again from what the server holds after a save. */}
      {!isLoading && !error && policy !== undefined && <PolicyForm key={policy?.updated_at ?? 'none'} policy={policy} />}
    </div>
  )
}

function PolicyForm({ policy }: { policy: DunningPolicy | null }) {
  const queryClient = useQueryClient()
  const [form, setForm] = useState({
    enabled: policy?.enabled ?? NEW_ACCOUNT_POLICY.enabled,
    retry_days: (policy?.retry_days ?? NEW_ACCOUNT_POLICY.retry_days).join(', '),
    final_subscription_action: policy?.final_subscription_action ?? NEW_ACCOUNT_POLICY.final_subscription_action,
    final_invoice_action: policy?.final_invoice_action ?? NEW_ACCOUNT_POLICY.final_invoice_action,
  })
  const [saving, setSaving] = useState(false)
  const schedule = parseRetryDays(form.retry_days)
  const retriesOff = retriesOffNotice(policy)

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (schedule.error) return
    setSaving(true)
    try {
      await putDunningPolicy({
        enabled: form.enabled,
        retry_days: schedule.days,
        final_subscription_action: form.final_subscription_action as DunningPolicyInputFinalSubscriptionAction,
        final_invoice_action: form.final_invoice_action as DunningPolicyInputFinalInvoiceAction,
      })
      await queryClient.invalidateQueries({ queryKey: getGetDunningPolicyQueryKey() })
      toast.success('Policy saved')
    } catch (err) {
      showApiError(err, 'Failed to save')
    } finally {
      setSaving(false)
    }
  }

  return (
    <>
      {retriesOff && (
        <Callout tone="amber" title={retriesOff.title} role="status">
          {retriesOff.body}
        </Callout>
      )}
      <Card className="max-w-md">
        <CardContent className="p-4">
          <form onSubmit={handleSubmit} noValidate className="space-y-4" aria-label="Retry policy">
            <div className="flex items-center justify-between">
              <Label>Retry failed payments</Label>
              <Switch checked={form.enabled} onCheckedChange={(val) => setForm(f => ({ ...f, enabled: val }))} />
            </div>
            <div className="space-y-2">
              <Label htmlFor="retry-days">Days before each retry</Label>
              <Input id="retry-days" value={form.retry_days} placeholder="3, 4, 7" inputMode="numeric"
                aria-invalid={!!schedule.error} aria-describedby="retry-days-help"
                onChange={e => setForm(f => ({ ...f, retry_days: e.target.value }))} />
              <p id="retry-days-help" className="text-xs text-muted-foreground">
                Separate with commas. The first wait counts from the failed payment, or from the collection that found no card on file. Each next one counts from the retry before it.{' '}
                {schedule.error
                  ? <span className="text-destructive">{schedule.error}</span>
                  : <span className="text-foreground">{retryPlanText(schedule.days)} after the failed payment or missing card.</span>}
              </p>
            </div>
            <fieldset className="space-y-3 rounded-md border p-3">
              <legend className="px-1 text-sm font-medium">When all retries fail</legend>
              <div className="space-y-2">
                <Label>Do this to the subscription</Label>
                <Select items={SUBSCRIPTION_ACTION_OPTIONS} value={form.final_subscription_action} onValueChange={(val) => setForm(f => ({ ...f, final_subscription_action: val ?? 'pause' }))}>
                  <SelectTrigger className="w-full">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {SUBSCRIPTION_ACTION_OPTIONS.map(o => (
                      <SelectItem key={o.value} value={o.value}>{o.label}</SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-2">
                <Label>Do this to the unpaid invoice</Label>
                <Select items={INVOICE_ACTION_OPTIONS} value={form.final_invoice_action} onValueChange={(val) => setForm(f => ({ ...f, final_invoice_action: val ?? 'none' }))}>
                  <SelectTrigger className="w-full">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {INVOICE_ACTION_OPTIONS.map(o => (
                      <SelectItem key={o.value} value={o.value}>{o.label}</SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <p className="text-xs text-muted-foreground">
                {outcomeSentence(form.final_subscription_action, form.final_invoice_action)}
              </p>
            </fieldset>
            <div className="flex justify-end">
              <Button type="submit" disabled={saving || !!schedule.error}>
                {saving ? 'Saving…' : 'Save'}
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>
    </>
  )
}
