import { useState } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'
import { toast } from 'sonner'
import { api, formatDate, type ApiKeyInfo } from '@/lib/api'
import { timeAgo, sinceMs, untilMs, daysUntil, wallClockNow, type EffectiveNow } from '@/lib/effectiveNow'
import { addDaysInTZ, endOfDayInTZ, formatExpiryHintInTZ } from '@/lib/dates'
import { applyApiError, showApiError } from '@/lib/formErrors'
import { StatusBadge } from '@/components/StatusBadge'
import { NoticeRow } from '@/components/NoticeRow'
import type { StatusDisplay } from '@/lib/statusLabels'
import { Layout } from '@/components/Layout'
import { cn } from '@/lib/utils'

import { Button } from '@/components/ui/button'
import { LoadError } from '@/components/LoadError'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { DatePicker } from '@/components/ui/date-picker'
import { Card, CardContent } from '@/components/ui/card'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import {
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { CardListSkeleton } from '@/components/ui/TableSkeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'

import { Plus, Key, Shield, ChevronDown, Loader2, RotateCw, MoreHorizontal } from 'lucide-react'
import { EmptyState } from '@/components/EmptyState'

// The list standard (UI plan 1.4): 12 px muted headers on a row shorter than
// the data rows, and rows tall enough to tap on a phone (1.14).
const HEAD = 'h-9 text-xs font-medium text-muted-foreground'
const ROW = 'h-11'

const createApiKeySchema = z.object({
  name: z.string().min(1, 'Name is required'),
})

type CreateApiKeyData = z.infer<typeof createApiKeySchema>

// API keys are platform/tenant-scoped and never clock-pinned, so their
// timestamps are genuinely wall-clock — callers pass wallClockNow(). The
// required anchor keeps the choice explicit rather than an implicit Date.now().
function relativeTime(dateStr: string, now: EffectiveNow): string {
  const days = Math.floor(sinceMs(dateStr, now) / 86_400_000)
  if (days < 7) return timeAgo(dateStr, now)
  return formatDate(dateStr)
}

// Human label for a key type — the raw enum ("secret") is a backend
// identifier, not operator copy.
function keyTypeLabel(type: string): string {
  switch (type) {
    case 'secret': return 'Secret'
    case 'platform': return 'Platform'
    default: return type
  }
}

function isExpired(key: ApiKeyInfo): boolean {
  return !!key.expires_at && untilMs(key.expires_at, wallClockNow()) < 0
}

// keyStatus is the one status word a key shows: Active, a warning in the
// last week before it expires, Expired or Revoked.
function keyStatus(key: ApiKeyInfo): StatusDisplay {
  if (key.revoked_at) return { label: 'Revoked', tone: 'neutral' }
  if (isExpired(key)) return { label: 'Expired', tone: 'neutral' }
  if (key.expires_at) {
    const days = daysUntil(key.expires_at, wallClockNow())
    if (days <= 7) return { label: days === 0 ? 'Expires today' : `Expires in ${days} day${days === 1 ? '' : 's'}`, tone: 'warning' }
  }
  return { label: 'Active', tone: 'success' }
}

function inactiveLabel(expired: number, revoked: number): string {
  const parts: string[] = []
  if (expired > 0) parts.push(`${expired} expired`)
  if (revoked > 0) parts.push(`${revoked} revoked`)
  return `${parts.join(' and ')} key${expired + revoked === 1 ? '' : 's'}`
}

export default function ApiKeysPage() {
  usePageTitle('API keys')
  const [showCreate, setShowCreate] = useState(false)
  const [createdKey, setCreatedKey] = useState<string | null>(null)
  const [revokeTarget, setRevokeTarget] = useState<ApiKeyInfo | null>(null)
  const [revoking, setRevoking] = useState(false)
  const [rotateTarget, setRotateTarget] = useState<ApiKeyInfo | null>(null)
  const [showInactive, setShowInactive] = useState(false)
  const queryClient = useQueryClient()

  // Tenant timezone for displaying expires_at — see CreateKeyDialog
  // for the "why tenant TZ" rationale. UTC fallback covers the
  // before-fetch race; the column is NOT NULL with default 'UTC' so
  // every loaded settings row has a real value.
  const { data: tenantSettings } = useQuery({
    queryKey: ['settings'],
    queryFn: () => api.getSettings(),
  })
  const tenantTZ = tenantSettings?.timezone || 'UTC'

  const { data: keysData, isLoading: loading, error: loadError, refetch } = useQuery({
    queryKey: ['api-keys'],
    queryFn: () => api.listApiKeys(),
  })

  const keys = keysData?.data ?? []

  const handleRevoke = async () => {
    if (!revokeTarget || revoking) return
    setRevoking(true)
    try {
      await api.revokeApiKey(revokeTarget.id)
      toast.success('API key revoked')
      setRevokeTarget(null)
      queryClient.invalidateQueries({ queryKey: ['api-keys'] })
    } catch (err) {
      showApiError(err, 'Failed to revoke key')
    } finally {
      setRevoking(false)
    }
  }

  const activeKeys = keys.filter(k => !k.revoked_at && !isExpired(k))
  const expiredKeys = keys.filter(k => !k.revoked_at && isExpired(k))
  const revokedKeys = keys.filter(k => !!k.revoked_at)

  return (
    <Layout>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="min-w-0 flex-1 basis-72">
          <h1 className="text-2xl font-semibold text-foreground">API keys</h1>
          <p className="text-sm text-muted-foreground mt-1">Keys your app uses to call the Hardcount API{activeKeys.length > 0 ? ` · ${activeKeys.length} active` : ''}</p>
        </div>
        <Button size="sm" onClick={() => setShowCreate(true)}>
          <Plus size={16} className="mr-2" />
          Create API key
        </Button>
      </div>

      {/* Said once for every key, not on each row. */}
      {keys.length > 0 && (
        <NoticeRow
          className="mt-4"
          icon={Shield}
          title="Keep secret keys on your server"
        >
          A secret key has full access. Never put one in browser or mobile app code.
        </NoticeRow>
      )}

      {loadError ? (
        <Card className="mt-6">
          <CardContent className="p-0">
            <LoadError what="API keys" error={loadError} onRetry={() => refetch()} />
          </CardContent>
        </Card>
      ) : loading ? (
        <div className="mt-6">
          <CardListSkeleton rows={3} />
        </div>
      ) : keys.length === 0 ? (
        <Card className="mt-6">
          <CardContent className="p-0">
            <EmptyState
              icon={Key}
              title="No API keys yet"
              description="Create an API key to start using the Hardcount API from your backend or CLI."
              action={{
                label: 'Create API key',
                icon: Plus,
                onClick: () => setShowCreate(true),
              }}
            />
          </CardContent>
        </Card>
      ) : (
        <>
          {/* One row per key (UI plan 4.22): the type, the key's start, its
              status and dates, and the actions in the row's menu. Per
              ADR-011 dashboard sessions are user-bound, not key-bound, so
              every active key can be revoked (the server still enforces
              permissions). */}
          <Card className="mt-6">
            <CardContent className="p-0">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead className={HEAD}>Name</TableHead>
                    <TableHead className={cn(HEAD, "hidden md:table-cell")}>Type</TableHead>
                    <TableHead className={cn(HEAD, "hidden md:table-cell")}>Key</TableHead>
                    <TableHead className={HEAD}>Status</TableHead>
                    <TableHead className={cn(HEAD, "hidden md:table-cell")}>Expires</TableHead>
                    <TableHead className={cn(HEAD, "hidden md:table-cell")}>Last used</TableHead>
                    <TableHead className={cn(HEAD, "w-12")}><span className="sr-only">Actions</span></TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {[...activeKeys, ...(showInactive ? [...expiredKeys, ...revokedKeys] : [])].map(k => {
                    const revoked = !!k.revoked_at
                    const expired = !revoked && isExpired(k)
                    return (
                      <TableRow key={k.id} className={cn(ROW, (revoked || expired) && 'text-muted-foreground')}>
                        <TableCell className={cn('text-sm font-medium', revoked || expired ? 'text-muted-foreground' : 'text-foreground')}>
                          {k.name}
                        </TableCell>
                        <TableCell className="hidden md:table-cell text-sm text-muted-foreground">{keyTypeLabel(k.key_type)}</TableCell>
                        <TableCell className="hidden md:table-cell">
                          <code className="text-xs font-mono text-muted-foreground">{k.key_prefix}…</code>
                        </TableCell>
                        <TableCell><StatusBadge status={keyStatus(k)} /></TableCell>
                        <TableCell className="hidden md:table-cell text-sm text-muted-foreground">
                          {k.expires_at ? formatDate(k.expires_at, tenantTZ) : 'Never'}
                        </TableCell>
                        <TableCell className="hidden md:table-cell text-sm text-muted-foreground">
                          {k.last_used_at ? relativeTime(k.last_used_at, wallClockNow()) : 'Never'}
                        </TableCell>
                        <TableCell className="text-right">
                          {!revoked && (
                            <DropdownMenu>
                              <DropdownMenuTrigger render={<Button variant="ghost" size="icon" className="size-10 md:size-8" aria-label={`Actions for ${k.name}`} />}>
                                <MoreHorizontal size={16} />
                              </DropdownMenuTrigger>
                              <DropdownMenuContent align="end" className="w-40">
                                {!expired && (
                                  <DropdownMenuItem onClick={() => setRotateTarget(k)}>
                                    <RotateCw size={14} className="mr-2" />
                                    Rotate
                                  </DropdownMenuItem>
                                )}
                                <DropdownMenuItem onClick={() => setRevokeTarget(k)} className="text-destructive focus:text-destructive">
                                  Revoke
                                </DropdownMenuItem>
                              </DropdownMenuContent>
                            </DropdownMenu>
                          )}
                        </TableCell>
                      </TableRow>
                    )
                  })}
                </TableBody>
              </Table>
            </CardContent>
          </Card>

          {expiredKeys.length + revokedKeys.length > 0 && (
            <button onClick={() => setShowInactive(!showInactive)}
              className="mt-3 flex items-center gap-2 text-sm text-muted-foreground hover:text-foreground transition-colors">
              <ChevronDown size={14} className={cn('transition-transform', showInactive && 'rotate-180')} />
              {showInactive ? 'Hide' : 'Show'} {inactiveLabel(expiredKeys.length, revokedKeys.length)}
            </button>
          )}
        </>
      )}

      {/* Create key dialog */}
      {showCreate && (
        <CreateKeyDialog
          onClose={() => setShowCreate(false)}
          onCreated={(rawKey) => {
            setShowCreate(false)
            setCreatedKey(rawKey)
            queryClient.invalidateQueries({ queryKey: ['api-keys'] })
            toast.success('API key created')
          }}
        />
      )}

      {/* Rotate key dialog */}
      {rotateTarget && (
        <RotateKeyDialog
          target={rotateTarget}
          onClose={() => setRotateTarget(null)}
          onRotated={(rawKey) => {
            setRotateTarget(null)
            setCreatedKey(rawKey)
            queryClient.invalidateQueries({ queryKey: ['api-keys'] })
            toast.success('API key rotated')
          }}
        />
      )}

      {/* Show created key */}
      {createdKey && (
        <Dialog open onOpenChange={() => setCreatedKey(null)}>
          <DialogContent className="sm:max-w-md">
            <DialogHeader>
              <DialogTitle>API key created</DialogTitle>
            </DialogHeader>
            <div className="space-y-4">
              <div className="bg-amber-50 dark:bg-amber-500/10 border border-amber-200 dark:border-amber-500/20 rounded-xl p-4">
                <div className="flex items-start gap-3">
                  <div className="w-8 h-8 rounded-lg bg-amber-100 dark:bg-amber-500/20 flex items-center justify-center shrink-0">
                    <Key size={16} className="text-amber-600" />
                  </div>
                  <div className="flex-1 min-w-0">
                    <p className="text-xs font-semibold text-amber-800 dark:text-amber-400">Save this key now. It won&apos;t be shown again.</p>
                    <div className="flex items-start gap-2 mt-2">
                      <code className="font-mono text-sm text-amber-900 dark:text-amber-300 break-all select-all flex-1 bg-amber-100/50 dark:bg-amber-500/10 rounded px-2 py-1">
                        {createdKey}
                      </code>
                      <Button variant="outline" size="sm" className="shrink-0"
                        onClick={() => {
                          navigator.clipboard.writeText(createdKey)
                          toast.success('Copied to clipboard')
                        }}>
                        Copy
                      </Button>
                    </div>
                  </div>
                </div>
              </div>
              <DialogFooter>
                <Button onClick={() => setCreatedKey(null)}>I've saved this key</Button>
              </DialogFooter>
            </div>
          </DialogContent>
        </Dialog>
      )}

      {/* Revoke confirmation */}
      <AlertDialog open={!!revokeTarget} onOpenChange={(open) => { if (!open) setRevokeTarget(null) }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Revoke API key?</AlertDialogTitle>
            {/* Key lookup reads only rows with revoked_at IS NULL and
                nothing caches keys (internal/auth), so the next request
                with this key is refused. */}
            <AlertDialogDescription>
              {revokeTarget
                ? `Anything using "${revokeTarget.name}" (${revokeTarget.key_prefix}…) is refused from its next request. This can't be undone.`
                : ''}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel onClick={() => setRevokeTarget(null)}>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={handleRevoke} disabled={revoking} className="bg-destructive text-destructive-foreground hover:bg-destructive/90">
              {revoking ? 'Revoking…' : 'Revoke key'}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Layout>
  )
}

/* --- Create key Dialog --- */

type ExpiryPreset = 'never' | '30d' | '90d' | '1y' | 'custom'

// Date-grade helpers (endOfDayInTZ, addDaysInTZ, formatExpiryHintInTZ)
// live in @/lib/dates so Credits / future pickers can reuse
// the same TZ-grounded arithmetic. See ADR-010.

function CreateKeyDialog({ onClose, onCreated }: { onClose: () => void; onCreated: (rawKey: string) => void }) {
  const [expiryPreset, setExpiryPreset] = useState<ExpiryPreset>('never')
  const [customDate, setCustomDate] = useState('')
  const [customDateError, setCustomDateError] = useState('')

  // Tenant timezone is the canonical clock for date-grade picks
  // (matches Stripe / Auth0). Fetch from /v1/settings — already
  // exposed there. Fall back to UTC if settings haven't loaded yet
  // (covers the dialog-open-before-fetch race; UTC is the DB
  // default so no tenant operates without a value in practice).
  const { data: settings } = useQuery({
    queryKey: ['settings'],
    queryFn: () => api.getSettings(),
  })
  const tenantTZ = settings?.timezone || 'UTC'

  const form = useForm<CreateApiKeyData>({
    resolver: zodResolver(createApiKeySchema),
    defaultValues: { name: '' },
  })

  function getExpiresAt(): string | undefined {
    switch (expiryPreset) {
      case '30d': return addDaysInTZ(30, tenantTZ)
      case '90d': return addDaysInTZ(90, tenantTZ)
      case '1y': return addDaysInTZ(365, tenantTZ)
      case 'custom': return customDate ? endOfDayInTZ(customDate, tenantTZ) : undefined
      default: return undefined
    }
  }

  const onSubmit = form.handleSubmit(async (data) => {
    setCustomDateError('')
    if (expiryPreset === 'custom' && !customDate) {
      setCustomDateError('Please select an expiration date')
      return
    }
    try {
      const res = await api.createApiKey({
        name: data.name,
        key_type: 'secret',
        expires_at: getExpiresAt(),
      })
      onCreated(res.raw_key)
    } catch (err) {
      applyApiError(form, err, ['name', 'key_type', 'expires_at'], {
        toastTitle: 'Failed to create API key',
      })
    }
  })

  const presets: { value: ExpiryPreset; label: string }[] = [
    { value: 'never', label: 'No expiration' },
    { value: '30d', label: '30 days' },
    { value: '90d', label: '90 days' },
    { value: '1y', label: '1 year' },
    { value: 'custom', label: 'Custom' },
  ]

  // Min date for custom picker = tomorrow at start-of-day. Without
  // setHours(0,0,0,0), `tomorrow` carries the current wall-clock time
  // and the DayPicker's `before: minDate` matcher inconsistently
  // disables today depending on time-of-day — so today could leak
  // through. A 0-day-from-now expiry is also a foot-gun for prod
  // workflows, hence the tomorrow-or-later floor (matches GitHub PAT
  // minimum and the FLOW K2 doc bullet).
  const tomorrow = new Date(wallClockNow())
  tomorrow.setDate(tomorrow.getDate() + 1)
  tomorrow.setHours(0, 0, 0, 0)

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Create API key</DialogTitle>
          <DialogDescription>Generate a new key for programmatic API access</DialogDescription>
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
                    <Input placeholder="e.g. Production, Staging, CI/CD" maxLength={100} {...field} />
                  </FormControl>
                  <FormDescription>A descriptive name to identify this key</FormDescription>
                  <FormMessage />
                </FormItem>
              )}
            />

            <div>
              <Label className="mb-2 block">Key type</Label>
              <div className="flex items-start gap-3 p-3 rounded-xl border border-border">
                <Shield size={18} className="mt-0.5 text-primary" />
                <div>
                  <p className="text-sm font-medium text-foreground">Secret</p>
                  <p className="text-xs text-muted-foreground mt-0.5">Full access. Keep it on your server.</p>
                </div>
              </div>
            </div>

            <div>
              <Label className="mb-2 block">Expiration</Label>
              <div className="flex flex-wrap gap-2">
                {presets.map(p => (
                  <button
                    key={p.value}
                    type="button"
                    onClick={() => setExpiryPreset(p.value)}
                    className={cn(
                      'px-3 py-1.5 rounded-lg text-sm border transition-colors',
                      expiryPreset === p.value
                        ? 'border-primary bg-primary/5 text-primary font-medium'
                        : 'border-border text-muted-foreground hover:text-foreground hover:border-border/80'
                    )}
                  >
                    {p.label}
                  </button>
                ))}
              </div>
              {expiryPreset === 'custom' && (
                <div className="mt-3">
                  <DatePicker
                    value={customDate}
                    onChange={(d) => { setCustomDate(d); setCustomDateError('') }}
                    placeholder="Select expiration date"
                    className="w-56"
                    minDate={tomorrow}
                  />
                </div>
              )}
              {expiryPreset !== 'never' && expiryPreset !== 'custom' && (
                <p className="text-xs text-muted-foreground mt-2">
                  Key will expire on {formatExpiryHintInTZ(getExpiresAt()!, tenantTZ)}
                </p>
              )}
              {expiryPreset === 'custom' && customDate && (
                <p className="text-xs text-muted-foreground mt-2">
                  Key will expire on {formatExpiryHintInTZ(getExpiresAt()!, tenantTZ)}
                </p>
              )}
              {customDateError && <p className="text-destructive text-sm mt-2">{customDateError}</p>}
            </div>

            <DialogFooter>
              <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
              <Button type="submit" disabled={form.formState.isSubmitting}>
                {form.formState.isSubmitting ? (
                  <><Loader2 size={14} className="animate-spin mr-2" /> Creating...</>
                ) : (
                  'Create key'
                )}
              </Button>
            </DialogFooter>
          </form>
        </Form>
      </DialogContent>
    </Dialog>
  )
}

/* --- Rotate key Dialog --- */

// Rotate replaces a key in place: the old key keeps working for the
// chosen grace window (so deployed clients can swap credentials without
// downtime), then expires automatically. Picking "Now" revokes the old
// key the moment the new one is minted — pick this only when you can
// redeploy immediately. Mirrors Stripe's Roll API key UX.
const ROTATE_GRACE_PRESETS: { value: number; label: string }[] = [
  { value: 0, label: 'Now' },
  { value: 60 * 60, label: 'In 1 hour' },
  { value: 24 * 60 * 60, label: 'In 24 hours' },
  { value: 7 * 24 * 60 * 60, label: 'In 7 days' },
]

function RotateKeyDialog({
  target,
  onClose,
  onRotated,
}: {
  target: ApiKeyInfo
  onClose: () => void
  onRotated: (rawKey: string) => void
}) {
  const [grace, setGrace] = useState<number>(24 * 60 * 60)
  const [submitting, setSubmitting] = useState(false)

  const handleSubmit = async () => {
    setSubmitting(true)
    try {
      const res = await api.rotateApiKey(target.id, grace)
      onRotated(res.raw_key)
    } catch (err) {
      showApiError(err, 'Failed to rotate key')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open && !submitting) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Rotate API key</DialogTitle>
          <DialogDescription>
            A new {keyTypeLabel(target.key_type).toLowerCase()} key will be issued for "{target.name}". The current key
            stops working when the grace period ends — immediately if you pick "Now" — so anything
            still using it must switch to the new key within that window.
          </DialogDescription>
        </DialogHeader>

        <div>
          <Label className="mb-2 block">Expire current key</Label>
          <div className="flex flex-wrap gap-2">
            {ROTATE_GRACE_PRESETS.map(p => (
              <button
                key={p.value}
                type="button"
                onClick={() => setGrace(p.value)}
                className={cn(
                  'px-3 py-1.5 rounded-lg text-sm border transition-colors',
                  grace === p.value
                    ? 'border-primary bg-primary/5 text-primary font-medium'
                    : 'border-border text-muted-foreground hover:text-foreground hover:border-border/80'
                )}
              >
                {p.label}
              </button>
            ))}
          </div>
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={submitting}>Cancel</Button>
          <Button onClick={handleSubmit} disabled={submitting}>
            {submitting ? <><Loader2 size={14} className="animate-spin mr-2" /> Rotating...</> : 'Rotate key'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
