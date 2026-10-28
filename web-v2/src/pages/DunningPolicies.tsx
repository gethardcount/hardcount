import { useState } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { Pencil, Trash2, Plus, Star, AlertTriangle } from 'lucide-react'

import { api, type DunningPolicy, type DunningPolicyWithCount } from '@/lib/api'
import { Layout } from '@/components/Layout'
import { EmptyState } from '@/components/EmptyState'
import { LoadError } from '@/components/LoadError'
import { FeedSkeleton } from '@/components/ui/TableSkeleton'
import { showApiError } from '@/lib/formErrors'

import { Button } from '@/components/ui/button'
import { AlertDialog, AlertDialogContent, AlertDialogHeader, AlertDialogTitle, AlertDialogDescription, AlertDialogFooter, AlertDialogCancel } from '@/components/ui/alert-dialog'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Label } from '@/components/ui/label'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import { finalActionSummary } from '@/lib/dunningActions'
import { newPolicyDefaultNote, retriesOffNotice, SEEDED_POLICY_NOTE } from '@/lib/dunningCoverage'
import { Callout } from '@/components/Callout'
import { parseRetryDays, retryPlanText } from '@/lib/dunningSchedule'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'

// DunningPoliciesPage — admin surface for the campaigns model
// (ADR-036). Operators create named policies ("Enterprise — Net 30",
// "SMB — fast-fail"), edit them, mark one as the tenant default,
// delete unused ones. Customer assignment happens on the customer
// detail page (one dropdown picks from this list).
//
// Industry shape verified against Stripe (account-level retry policy),
// Lago (campaigns + per-customer assignment), Orb (rules + schedules),
// Recurly (campaigns + per-account assignment). Hardcount follows the
// converging named-templates pattern.

// ADR-036 amendment — four Stripe-aligned terminal actions. Single
// source of truth for both the dropdown <SelectItem>s and the Base UI
// `items` prop that lets <SelectValue> render the selected label
// (Base UI shows the raw value otherwise). Labels reflect the actual
// semantics (pause = collection-only, not hard pause).
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

export default function DunningPoliciesPage() {
  usePageTitle('Dunning policies')
  const queryClient = useQueryClient()
  const [editingPolicy, setEditingPolicy] = useState<DunningPolicyWithCount | null>(null)
  const [showCreate, setShowCreate] = useState(false)
  const [deleteTarget, setDeleteTarget] = useState<DunningPolicyWithCount | null>(null)

  const { data: policiesData, isLoading, error: policiesError, refetch: refetchPolicies } = useQuery({
    queryKey: ['dunning-policies'],
    queryFn: () => api.listDunningPolicies(),
  })
  const policies = policiesData?.data ?? []
  // With no policies the empty state below already says nothing is retried.
  const retriesOff = !isLoading && !policiesError && policies.length > 0 ? retriesOffNotice(policies) : null

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ['dunning-policies'] })

  async function handleSetDefault(id: string) {
    try {
      await api.setDefaultDunningPolicy(id)
      invalidate()
      toast.success('Default policy updated')
    } catch (err) {
      showApiError(err, 'Failed to set default')
    }
  }

  async function handleDelete(p: DunningPolicyWithCount) {
    if (p.is_default) {
      toast.error('Promote another policy to default first')
      return
    }
    if (p.assigned_customers > 0) {
      toast.error(`${p.assigned_customers} customer(s) assigned — reassign them first`)
      return
    }
    setDeleteTarget(p)
  }

  async function confirmDelete() {
    if (!deleteTarget) return
    try {
      await api.deleteDunningPolicy(deleteTarget.id)
      invalidate()
      toast.success('Policy deleted')
    } catch (err) {
      showApiError(err, 'Failed to delete')
    } finally {
      setDeleteTarget(null)
    }
  }

  return (
    <Layout>
      <div className="space-y-6">
        <div className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
          <div className="max-w-3xl">
            <h1 className="text-2xl font-semibold text-foreground">Dunning policies</h1>
            <p className="text-sm text-muted-foreground mt-1">
              A policy sets how many days to wait before each retry of a failed payment, and what happens if every retry fails. Your default policy applies to every customer unless you pick another on their page.
            </p>
          </div>
          <Button className="shrink-0 self-start" onClick={() => setShowCreate(true)}>
            <Plus size={16} className="mr-2" /> New policy
          </Button>
        </div>

        {retriesOff && (
          <Callout tone="amber" title={retriesOff.title} role="status">
            {retriesOff.body}
          </Callout>
        )}

        <Card>
          <CardHeader>
            <CardTitle className="text-sm">Policies ({policies.length})</CardTitle>
          </CardHeader>
          <CardContent className="p-0">
            {isLoading && <FeedSkeleton rows={3} />}
            {!isLoading && policiesError && (
              <LoadError what="dunning policies" error={policiesError} onRetry={() => refetchPolicies()} />
            )}
            {!isLoading && !policiesError && policies.length === 0 && (
              <EmptyState
                icon={AlertTriangle}
                title="No dunning policies yet"
                description="A dunning policy decides how failed payments are retried and when customers are emailed. Until you create one, failed payments are not retried."
                action={{ label: 'New policy', onClick: () => setShowCreate(true), icon: Plus }}
              />
            )}
            <div className="divide-y divide-border">
              {policies.map(p => (
                <div key={p.id} className="px-6 py-4 flex items-start justify-between gap-3">
                  <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-2 flex-wrap">
                      <p className="text-sm font-medium text-foreground">{p.name || '(unnamed policy)'}</p>
                      {p.is_default && <Badge variant="default"><Star size={10} className="mr-1" /> default</Badge>}
                      {!p.enabled && <Badge variant="outline">disabled</Badge>}
                      <Badge variant="outline">{p.assigned_customers} assigned</Badge>
                    </div>
                    <div className="mt-1 flex flex-col gap-y-1 text-xs text-muted-foreground sm:flex-row sm:flex-wrap sm:gap-x-6">
                      <span className="text-foreground">{retryPlanText(p.retry_days)}</span>
                      <span>On exhaustion: <span className="text-foreground">{finalActionSummary(p)}</span></span>
                    </div>
                    {p.seeded && (
                      <p className="mt-2 text-xs text-muted-foreground max-w-prose">{SEEDED_POLICY_NOTE}</p>
                    )}
                  </div>
                  <div className="flex items-center gap-2 shrink-0">
                    {!p.is_default && (
                      <Button variant="ghost" size="sm" onClick={() => handleSetDefault(p.id)}>
                        <Star size={14} className="mr-1" /> Make default
                      </Button>
                    )}
                    {/* Icon-only actions carry the policy name in their
                        accessible name. Without it both read as bare
                        "button" — and on a list of N policies that gives a
                        screen-reader user no way to tell which row's
                        DESTRUCTIVE action they are on. */}
                    <Button variant="ghost" size="sm" aria-label={`Edit policy ${p.name}`} onClick={() => setEditingPolicy(p)}>
                      <Pencil size={14} />
                    </Button>
                    {!p.is_default && (
                      <Button variant="ghost" size="sm" aria-label={`Delete policy ${p.name}`} onClick={() => handleDelete(p)}>
                        <Trash2 size={14} className="text-destructive" />
                      </Button>
                    )}
                  </div>
                </div>
              ))}
            </div>
          </CardContent>
        </Card>
      </div>

      {showCreate && (
        <PolicyDialog
          mode="create"
          policies={policies}
          onClose={() => setShowCreate(false)}
          onSaved={(p) => {
            setShowCreate(false)
            invalidate()
            toast.success(p?.is_default ? 'Policy created and made your default' : 'Policy created')
          }}
        />
      )}
      {editingPolicy && (
        <PolicyDialog
          mode="edit"
          initial={editingPolicy}
          onClose={() => setEditingPolicy(null)}
          onSaved={() => { setEditingPolicy(null); invalidate(); toast.success('Policy saved') }}
        />
      )}
      <AlertDialog open={!!deleteTarget} onOpenChange={(open) => { if (!open) setDeleteTarget(null) }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete policy &ldquo;{deleteTarget?.name}&rdquo;?</AlertDialogTitle>
            <AlertDialogDescription>
              The policy is removed permanently. Customers fall back to your default policy for future failed payments.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Keep policy</AlertDialogCancel>
            <Button variant="destructive" onClick={confirmDelete}>Delete policy</Button>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Layout>
  )
}

function PolicyDialog({ mode, initial, policies = [], onClose, onSaved }: {
  mode: 'create' | 'edit'
  initial?: DunningPolicy
  policies?: DunningPolicy[]
  onClose: () => void
  onSaved: (saved?: DunningPolicy) => void
}) {
  const [form, setForm] = useState({
    name: initial?.name ?? '',
    enabled: initial?.enabled ?? true,
    // The seeded default's schedule: day 3, 7 and 14.
    retry_days: (initial?.retry_days ?? [3, 4, 7]).join(', '),
    final_subscription_action: initial?.final_subscription_action || 'pause',
    final_invoice_action: initial?.final_invoice_action || 'none',
  })
  const [saving, setSaving] = useState(false)
  const schedule = parseRetryDays(form.retry_days)

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (schedule.error) return
    setSaving(true)
    try {
      const payload: Partial<DunningPolicy> = {
        name: form.name.trim(),
        enabled: form.enabled,
        retry_days: schedule.days,
        final_subscription_action: form.final_subscription_action,
        final_invoice_action: form.final_invoice_action,
      }
      let saved: DunningPolicy | undefined
      if (mode === 'create') {
        saved = await api.createDunningPolicy(payload)
      } else if (initial) {
        saved = await api.updateDunningPolicy(initial.id, payload)
      }
      onSaved(saved)
    } catch (err) {
      showApiError(err, mode === 'create' ? 'Failed to create' : 'Failed to save')
    } finally {
      setSaving(false)
    }
  }

  // Saving a new policy can replace the account's seeded default (ADR-123);
  // say so before the operator saves, not only on the seed's card.
  const defaultNote = mode === 'create' ? newPolicyDefaultNote(policies, form.enabled) : null

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{mode === 'create' ? 'New dunning policy' : 'Edit policy'}</DialogTitle>
        </DialogHeader>
        <form onSubmit={handleSubmit} noValidate className="space-y-4">
          <div className="space-y-2">
            <Label>Name</Label>
            <Input value={form.name} placeholder="Enterprise — Net 30 grace"
              onChange={e => setForm(f => ({ ...f, name: e.target.value }))} />
          </div>
          <div className="flex items-center justify-between">
            <Label>Enabled</Label>
            <Switch checked={form.enabled} onCheckedChange={(val) => setForm(f => ({ ...f, enabled: val }))} />
          </div>
          {defaultNote && (
            <p className="text-xs text-muted-foreground" role="status">{defaultNote}</p>
          )}
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
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={saving || !!schedule.error}>
              {saving ? 'Saving…' : 'Save'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
