import { useEffect, useRef, useState } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { useTenantTimezone } from '@/hooks/useTenantTimezone'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'
import { toast } from 'sonner'
import { api, ApiError, formatDate, formatDateTime } from '@/lib/api'
import type { APIKey } from '@/lib/gen/schemas/aPIKey'
import { APIKeyStatus } from '@/lib/gen/schemas/aPIKeyStatus'
import { timeAgo, sinceMs, wallClockNow, type EffectiveNow } from '@/lib/effectiveNow'
import { addDaysInTZ, calendarDaysUntil, civilDateFrom, endOfDayInTZ, formatExpiryHintInTZ } from '@/lib/dates'
import { applyApiError, showApiError } from '@/lib/formErrors'
import { StatusBadge } from '@/components/StatusBadge'
import { NoticeRow } from '@/components/NoticeRow'
import type { StatusDisplay } from '@/lib/statusLabels'
import { TypedConfirmDialog } from '@/components/TypedConfirmDialog'
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

import { Plus, Key, Shield, Activity, ChevronDown, Loader2, RotateCw, MoreHorizontal } from 'lucide-react'
import { EmptyState } from '@/components/EmptyState'
import { CopyButton, ShowOnceSecretDialog } from '@/components/ShowOnceSecretDialog'
import { API_ORIGIN, DOCS_BASE } from '@/lib/apiOrigin'

// 12 px muted headers on a row shorter than the data rows, and rows tall
// enough to tap on a phone.
const HEAD = 'h-9 text-xs font-medium text-muted-foreground'
const ROW = 'h-11'

// A name is 1 to 100 characters once trimmed, counted as the server counts
// them: by code point, so an emoji is one character.
const MAX_NAME_CHARS = 100

const createApiKeySchema = z.object({
  name: z.string().trim().min(1, 'Name is required')
    .refine(s => [...s].length <= MAX_NAME_CHARS, `Name must be at most ${MAX_NAME_CHARS} characters`),
})

type CreateApiKeyData = z.infer<typeof createApiKeySchema>

// A key is never on a test clock, so its times are wall-clock: callers pass
// wallClockNow().
function relativeTime(dateStr: string, now: EffectiveNow): string {
  const days = Math.floor(sinceMs(dateStr, now) / 86_400_000)
  if (days < 7) return timeAgo(dateStr, now)
  return formatDate(dateStr)
}

// Human label for a key type — the raw enum ("secret", "ingest") is a
// backend identifier, not operator copy. An ingest key can only send usage
// events (ADR-133).
function keyTypeLabel(type: string): string {
  switch (type) {
    case 'secret': return 'Secret'
    case 'ingest': return 'Usage only'
    default: return type
  }
}

// The same type as a noun for running text: "a new usage-only key".
function keyTypeNoun(type: string): string {
  switch (type) {
    case 'secret': return 'secret key'
    case 'ingest': return 'usage-only key'
    default: return `${type} key`
  }
}

type KeyTypeChoice = 'secret' | 'ingest'

// The two key types the create dialog offers, in plain words.
const KEY_TYPES: { value: KeyTypeChoice; label: string; hint: string; icon: typeof Shield }[] = [
  { value: 'secret', label: 'Secret', hint: 'Full access. Keep it on your server.', icon: Shield },
  { value: 'ingest', label: 'Usage only', hint: 'Can only send usage events. Use it in LiteLLM or another gateway.', icon: Activity },
]

// keyStatus is the status word a key shows. The server decides the status.
// An active key whose expiry is less than a week away also says how many days
// are left, in calendar days of the account's zone, so the count agrees with
// the date in the Expires column. Once that expiry has passed the key says
// only Active, until the list is fetched again and the server says Expired.
function keyStatus(key: APIKey, timezone: string, now: EffectiveNow): StatusDisplay {
  if (key.status === 'revoked') return { label: 'Revoked', tone: 'neutral' }
  if (key.status === 'expired') return { label: 'Expired', tone: 'neutral' }
  if (key.expires_at && Date.parse(key.expires_at) > now) {
    const days = calendarDaysUntil(key.expires_at, now, timezone)
    if (days <= 7) return { label: days === 0 ? 'Expires today' : `Expires in ${days} day${days === 1 ? '' : 's'}`, tone: 'warning' }
  }
  return { label: 'Active', tone: 'success' }
}

// inactiveLabel names the keys behind the toggle: "1 expired key",
// "3 revoked keys", or "2 inactive keys (1 expired, 1 revoked)".
function inactiveLabel(expired: number, revoked: number): string {
  const total = expired + revoked
  const keys = `key${total === 1 ? '' : 's'}`
  if (revoked === 0) return `${expired} expired ${keys}`
  if (expired === 0) return `${revoked} revoked ${keys}`
  return `${total} inactive ${keys} (${expired} expired, ${revoked} revoked)`
}

const KNOWN_STATUSES: readonly string[] = Object.values(APIKeyStatus)

// OutOfDateError is a list the page cannot show because the page and the
// server are on different versions: a tab left open across an upgrade, or a
// replica not yet upgraded. Loading the page again is what fixes it.
class OutOfDateError extends Error {}

// listKeys refuses an answer with a status the page does not know: such a key
// would fit none of the page's lists and vanish from it. The operator is told
// to reload; the statuses go to the console.
async function listKeys() {
  const res = await api.listApiKeys()
  const unknown = [...new Set(res.data.map(k => k.status).filter(s => !KNOWN_STATUSES.includes(s)))]
  if (unknown.length > 0) {
    console.error(`API keys: the server sent status ${unknown.join(', ')}; this page knows ${KNOWN_STATUSES.join(', ')}`)
    throw new OutOfDateError('This page is out of date with the server. Reload it.')
  }
  return res
}

// oldKeySentence says what a rotation did to the old key, from the server's
// answer: revoked now, or working until its expiry. Anything else, nothing.
function oldKeySentence(old: APIKey, timezone: string): string {
  if (old.status === 'revoked') return 'The old key has stopped working.'
  if (old.status === 'active' && old.expires_at) return `The old key stops working at ${formatDateTime(old.expires_at, timezone)}.`
  return ''
}

// revokeSentence is what revoking a key does, the same in both dialogs.
function revokeSentence(key: APIKey): string {
  return `"${key.name}" (${key.key_prefix}…) stops working at once: every request made with it is refused. This can't be undone.`
}

export default function ApiKeysPage() {
  usePageTitle('API keys')
  const [showCreate, setShowCreate] = useState(false)
  const [createdKey, setCreatedKey] = useState<{ key: string; oldKey?: APIKey } | null>(null)
  const [revokeTarget, setRevokeTarget] = useState<APIKey | null>(null)
  const [revoking, setRevoking] = useState(false)
  const [rotateTarget, setRotateTarget] = useState<APIKey | null>(null)
  const [showInactive, setShowInactive] = useState(false)
  const queryClient = useQueryClient()

  // Tenant timezone for displaying expires_at.
  const tenantTZ = useTenantTimezone()

  const { data: keysData, isLoading: loading, error: loadError, refetch } = useQuery({
    queryKey: ['api-keys'],
    queryFn: listKeys,
  })

  const keys = keysData?.data ?? []

  // When an active key's expiry passes, fetch the list once so the server's
  // Expired replaces it: at that moment if it is within a day, or now for a
  // key already past it.
  const refetchedFor = useRef(new Set<string>())
  useEffect(() => {
    const due = (keysData?.data ?? []).filter(k => k.status === 'active' && k.expires_at && !refetchedFor.current.has(k.id))
    if (due.length === 0) return
    const soonest = Math.min(...due.map(k => Date.parse(k.expires_at!)))
    const wait = soonest - wallClockNow()
    if (wait > 86_400_000) return
    const timer = setTimeout(() => {
      const now = wallClockNow()
      due.filter(k => Date.parse(k.expires_at!) <= now).forEach(k => refetchedFor.current.add(k.id))
      void refetch()
    }, Math.max(0, wait) + 1000)
    return () => clearTimeout(timer)
  }, [keysData, refetch])

  const handleRevoke = async () => {
    if (!revokeTarget || revoking) return
    setRevoking(true)
    try {
      await api.revokeApiKey(revokeTarget.id)
      toast.success('API key revoked')
      setRevokeTarget(null)
      queryClient.invalidateQueries({ queryKey: ['api-keys'] })
    } catch (err) {
      // Someone revoked it first, in another tab or session: the list was out
      // of date, not the request.
      if (err instanceof ApiError && err.status === 404) {
        setRevokeTarget(null)
        toast.info('That key was already revoked.')
        queryClient.invalidateQueries({ queryKey: ['api-keys'] })
        return
      }
      showApiError(err, 'Failed to revoke key')
    } finally {
      setRevoking(false)
    }
  }

  const activeKeys = keys.filter(k => k.status === 'active')
  const expiredKeys = keys.filter(k => k.status === 'expired')
  const revokedKeys = keys.filter(k => k.status === 'revoked')
  const now = wallClockNow()

  const keyRow = (k: APIKey) => {
    const active = k.status === 'active'
    return (
      <TableRow key={k.id} className={cn(ROW, !active && 'text-muted-foreground')}>
        <TableCell className={cn('whitespace-normal text-sm font-medium [overflow-wrap:anywhere]', active ? 'text-foreground' : 'text-muted-foreground')}>
          {k.name}
        </TableCell>
        <TableCell className="hidden md:table-cell text-sm text-muted-foreground">{keyTypeLabel(k.key_type)}</TableCell>
        <TableCell className="hidden md:table-cell">
          <code className="text-xs font-mono text-muted-foreground">{k.key_prefix}…</code>
        </TableCell>
        <TableCell><StatusBadge status={keyStatus(k, tenantTZ, now)} /></TableCell>
        <TableCell className="hidden md:table-cell text-sm text-muted-foreground">
          {k.expires_at ? formatDate(k.expires_at, tenantTZ) : 'Never'}
        </TableCell>
        <TableCell className="hidden md:table-cell text-sm text-muted-foreground">
          {k.last_used_at ? relativeTime(k.last_used_at, now) : 'Never'}
        </TableCell>
        <TableCell className="text-right">
          {active && (
            <DropdownMenu>
              <DropdownMenuTrigger render={<Button variant="ghost" size="icon" className="size-10 md:size-8" aria-label={`Actions for ${k.name}`} />}>
                <MoreHorizontal size={16} />
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end" className="w-40">
                <DropdownMenuItem onClick={() => setRotateTarget(k)}>
                  <RotateCw size={14} className="mr-2" />
                  Rotate
                </DropdownMenuItem>
                <DropdownMenuItem onClick={() => setRevokeTarget(k)} className="text-destructive focus:text-destructive">
                  Revoke
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          )}
        </TableCell>
      </TableRow>
    )
  }

  return (
    <>
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

      <QuickStart />

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
            {loadError instanceof OutOfDateError
              ? <LoadError what="API keys" error={loadError} onRetry={() => window.location.reload()} retryLabel="Reload" />
              : <LoadError what="API keys" error={loadError} onRetry={() => refetch()} />}
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
          {/* One row per key: the type, the key's start, its status and
              dates, and the actions in the row's menu. Every active key can
              be revoked: a dashboard session is not tied to a key (ADR-011). */}
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
                  {activeKeys.length > 0 ? activeKeys.map(keyRow) : (
                    <TableRow className={ROW}>
                      <TableCell colSpan={7} className="whitespace-normal text-sm text-muted-foreground">
                        No active keys.{' '}
                        <Button variant="link" size="sm" className="h-auto p-0" onClick={() => setShowCreate(true)}>Create a key</Button>
                      </TableCell>
                    </TableRow>
                  )}
                </TableBody>
                {expiredKeys.length + revokedKeys.length > 0 && (
                  // Its own top border: the body above drops the border of its last row.
                  <TableBody id="api-key-inactive-rows" hidden={!showInactive} className="border-t">
                    {[...expiredKeys, ...revokedKeys].map(keyRow)}
                  </TableBody>
                )}
              </Table>
            </CardContent>
          </Card>

          {expiredKeys.length + revokedKeys.length > 0 && (
            <button onClick={() => setShowInactive(!showInactive)}
              aria-expanded={showInactive} aria-controls="api-key-inactive-rows"
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
            setCreatedKey({ key: rawKey })
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
          onRotated={(rawKey, oldKey) => {
            setRotateTarget(null)
            setCreatedKey({ key: rawKey, oldKey })
            queryClient.invalidateQueries({ queryKey: ['api-keys'] })
            toast.success('API key rotated')
          }}
        />
      )}

      {/* The new key, shown once. */}
      {createdKey && (
        <ShowOnceSecretDialog
          title={createdKey.oldKey ? 'API key rotated' : 'API key created'}
          label="New key"
          secret={createdKey.key}
          onDone={() => setCreatedKey(null)}
        >
          {createdKey.oldKey && oldKeySentence(createdKey.oldKey, tenantTZ) && (
            <p className="text-sm text-muted-foreground">{oldKeySentence(createdKey.oldKey, tenantTZ)}</p>
          )}
          <div>
            <p className="text-xs font-medium text-muted-foreground">Try it</p>
            <div className="mt-1.5 flex items-start gap-2">
              <code className="flex-1 break-all rounded bg-muted px-2 py-1 font-mono text-xs text-foreground">{tryItCurl(createdKey.key)}</code>
              <CopyButton text={tryItCurl(createdKey.key)} name="Copy the try-it command" />
            </div>
            <p className="mt-1.5 text-xs text-muted-foreground">It answers with the account and mode the key belongs to.</p>
          </div>
        </ShowOnceSecretDialog>
      )}

      {/* A live key runs production integrations, and revoking it breaks them
          at once, so it takes the typed word. A test key gets the plain
          confirm. */}
      {revokeTarget?.livemode && (
        <TypedConfirmDialog
          open
          onOpenChange={(open) => { if (!open) setRevokeTarget(null) }}
          title="Revoke live API key"
          description={revokeSentence(revokeTarget)}
          confirmWord="REVOKE"
          confirmLabel="Revoke key"
          onConfirm={handleRevoke}
          loading={revoking}
        />
      )}
      <AlertDialog open={!!revokeTarget && !revokeTarget.livemode} onOpenChange={(open) => { if (!open) setRevokeTarget(null) }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Revoke test API key</AlertDialogTitle>
            <AlertDialogDescription>{revokeTarget ? revokeSentence(revokeTarget) : ''}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel onClick={() => setRevokeTarget(null)}>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={handleRevoke} disabled={revoking} className="bg-destructive text-destructive-foreground hover:bg-destructive/90">
              {revoking ? 'Revoking…' : 'Revoke key'}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}

/* --- Create key Dialog --- */

type ExpiryPreset = 'never' | '30d' | '90d' | '1y' | 'custom'

// The first call to make with a key: whoami names its account and mode.
function tryItCurl(key: string): string {
  return `curl ${API_ORIGIN}/v1/whoami -H "Authorization: Bearer ${key}"`
}

// Where the API is and how to call it, before any key exists.
function QuickStart() {
  const guides = [
    { href: `${DOCS_BASE}/api-keys.md`, label: 'API keys' },
    { href: `${DOCS_BASE}/first-invoice.md`, label: 'Your first invoice' },
    { href: `${DOCS_BASE}/webhooks.md`, label: 'Webhooks' },
  ]
  return (
    <Card className="mt-4">
      <CardContent className="p-4 space-y-3">
        <div>
          <p className="text-sm font-medium text-foreground">Quick start</p>
          <p className="text-xs text-muted-foreground mt-0.5">
            Call the API at this address and send a key as <code className="font-mono">Authorization: Bearer &lt;key&gt;</code>.
          </p>
        </div>
        <div className="flex items-center gap-2">
          <code className="min-w-0 flex-1 truncate rounded bg-muted px-2 py-1.5 font-mono text-sm text-foreground" title={API_ORIGIN}>{API_ORIGIN}</code>
          <CopyButton text={API_ORIGIN} />
        </div>
        <p className="flex flex-wrap gap-x-4 gap-y-1 text-xs">
          <span className="text-muted-foreground">Guides:</span>
          {guides.map(g => (
            <a key={g.href} href={g.href} target="_blank" rel="noreferrer" className="text-primary underline-offset-2 hover:underline">{g.label}</a>
          ))}
        </p>
      </CardContent>
    </Card>
  )
}

function CreateKeyDialog({ onClose, onCreated }: { onClose: () => void; onCreated: (rawKey: string) => void }) {
  const [keyType, setKeyType] = useState<KeyTypeChoice>('secret')
  const [expiryPreset, setExpiryPreset] = useState<ExpiryPreset>('never')
  const [customDate, setCustomDate] = useState('')
  const [customDateError, setCustomDateError] = useState('')

  // Date picks use the account's timezone, from its settings.
  const tenantTZ = useTenantTimezone()

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
        key_type: keyType,
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

  // The custom picker starts tomorrow in the account's zone: a key that
  // expires later today would stop its integrations before anyone notices.
  const now = wallClockNow()
  const tomorrow = civilDateFrom(now, 1, tenantTZ)

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
                {KEY_TYPES.map(kt => {
                  const Icon = kt.icon
                  const selected = keyType === kt.value
                  return (
                    <button
                      key={kt.value}
                      type="button"
                      role="radio"
                      aria-checked={selected}
                      onClick={() => setKeyType(kt.value)}
                      className={cn(
                        'flex items-start gap-3 p-3 rounded-xl border text-left transition-colors',
                        selected ? 'border-primary bg-primary/5' : 'border-border hover:border-foreground/30'
                      )}
                    >
                      <Icon size={18} className={cn('mt-0.5 shrink-0', selected ? 'text-primary' : 'text-muted-foreground')} />
                      <span>
                        <span className="block text-sm font-medium text-foreground">{kt.label}</span>
                        <span className="block text-xs text-muted-foreground mt-0.5">{kt.hint}</span>
                      </span>
                    </button>
                  )
                })}
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
                    timeZone={tenantTZ}
                    now={now}
                    min={tomorrow}
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

// Rotate replaces a key: the old key keeps working for the chosen grace
// window, so deployed clients can switch, then expires. "Now" revokes the old
// key as the new one is made.
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
  target: APIKey
  onClose: () => void
  onRotated: (rawKey: string, oldKey: APIKey) => void
}) {
  const [grace, setGrace] = useState<number>(24 * 60 * 60)
  const [submitting, setSubmitting] = useState(false)

  const handleSubmit = async () => {
    setSubmitting(true)
    try {
      const res = await api.rotateApiKey(target.id, grace)
      onRotated(res.raw_key, res.old_key)
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
            A new {keyTypeNoun(target.key_type)} will be issued for "{target.name}". The old key
            stops working at the time you pick, so anything still using it must switch to the new
            key by then.
          </DialogDescription>
        </DialogHeader>

        <div>
          <Label className="mb-2 block">Old key stops working</Label>
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
