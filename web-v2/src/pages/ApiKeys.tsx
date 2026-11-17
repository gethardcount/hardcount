import { useEffect, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { toast } from 'sonner'
import { ChevronDown, Key, MoreHorizontal, Plus, RotateCw, Shield } from 'lucide-react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { useTenantTimezone } from '@/hooks/useTenantTimezone'
import { api, ApiError, formatDate, formatDateTime } from '@/lib/api'
import { API_ORIGIN, DOCS_BASE } from '@/lib/apiOrigin'
import { calendarDaysUntil } from '@/lib/dates'
import { sinceMs, timeAgo, wallClockNow, type EffectiveNow } from '@/lib/effectiveNow'
import { showApiError } from '@/lib/formErrors'
import type { APIKey } from '@/lib/gen/schemas/aPIKey'
import { APIKeyStatus } from '@/lib/gen/schemas/aPIKeyStatus'
import type { StatusDisplay } from '@/lib/statusLabels'
import { cn } from '@/lib/utils'
import { CreateKeyDialog } from '@/components/apikeys/CreateKeyDialog'
import { keyTypeLabel } from '@/components/apikeys/keyTypes'
import { RotateKeyDialog } from '@/components/apikeys/RotateKeyDialog'
import { EmptyState } from '@/components/EmptyState'
import { LoadError } from '@/components/LoadError'
import { NoticeRow } from '@/components/NoticeRow'
import { CopyButton, ShowOnceSecretDialog } from '@/components/ShowOnceSecretDialog'
import { StatusBadge } from '@/components/StatusBadge'
import { TypedConfirmDialog } from '@/components/TypedConfirmDialog'
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
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { CardListSkeleton } from '@/components/ui/TableSkeleton'

const HEAD = 'h-9 text-xs font-medium text-muted-foreground'
// Tall enough to tap on a phone.
const ROW = 'h-11'

// expiringIn is how many days an active key has left when that is 7 or fewer,
// and undefined otherwise. They are calendar days of the account's zone, so
// the count agrees with the date in the Expires column.
function expiringIn(key: APIKey, timezone: string, now: EffectiveNow): number | undefined {
  if (key.status !== 'active' || !key.expires_at || Date.parse(key.expires_at) <= now) return undefined
  const days = calendarDaysUntil(key.expires_at, now, timezone)
  return days <= 7 ? days : undefined
}

// keyStatus is the status word a key shows. The server decides the status: a
// key whose expiry has passed says Active until the list is fetched again.
function keyStatus(key: APIKey, timezone: string, now: EffectiveNow): StatusDisplay {
  if (key.status === 'revoked') return { label: 'Revoked', tone: 'neutral' }
  if (key.status === 'expired') return { label: 'Expired', tone: 'neutral' }
  const days = expiringIn(key, timezone, now)
  if (days === undefined) return { label: 'Active', tone: 'success' }
  return { label: days === 0 ? 'Expires today' : `Expires in ${days} day${days === 1 ? '' : 's'}`, tone: 'warning' }
}

// lastUsed says how long ago within a week, and the date after that.
function lastUsed(key: APIKey, now: EffectiveNow): string {
  if (!key.last_used_at) return 'Never'
  return sinceMs(key.last_used_at, now) < 7 * 86_400_000 ? timeAgo(key.last_used_at, now) : formatDate(key.last_used_at)
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
async function listKeys(): Promise<APIKey[]> {
  const { data: keys } = await api.listApiKeys()
  const unknown = [...new Set(keys.map(k => k.status).filter(s => !KNOWN_STATUSES.includes(s)))]
  if (unknown.length > 0) {
    console.error(`API keys: the server sent status ${unknown.join(', ')}; this page knows ${KNOWN_STATUSES.join(', ')}`)
    throw new OutOfDateError('This page is out of date with the server. Reload it.')
  }
  return keys
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
  return `"${key.name}" (${key.key_prefix}…) is refused from the next request; a request already running finishes. This can't be undone.`
}

export default function ApiKeysPage() {
  usePageTitle('API keys')
  const tenantTZ = useTenantTimezone()
  const [showCreate, setShowCreate] = useState(false)
  const [rotateTarget, setRotateTarget] = useState<APIKey | null>(null)
  const [revokeTarget, setRevokeTarget] = useState<APIKey | null>(null)
  const [revoking, setRevoking] = useState(false)
  const [newKey, setNewKey] = useState<{ title: string; rawKey: string; note?: string } | null>(null)
  const [showInactive, setShowInactive] = useState(false)

  const { data, isLoading, error, refetch } = useQuery({ queryKey: ['api-keys'], queryFn: listKeys })

  // When an active key's expiry passes, fetch the list once so the server's
  // Expired replaces it: at that moment if it is within a day, or now for a
  // key already past it.
  const refetchedFor = useRef(new Set<string>())
  useEffect(() => {
    const due = (data ?? []).filter(k => k.status === 'active' && k.expires_at && !refetchedFor.current.has(k.id))
    if (due.length === 0) return
    const wait = Math.min(...due.map(k => Date.parse(k.expires_at!))) - wallClockNow()
    if (wait > 86_400_000) return
    const timer = setTimeout(() => {
      const now = wallClockNow()
      due.filter(k => Date.parse(k.expires_at!) <= now).forEach(k => refetchedFor.current.add(k.id))
      void refetch()
    }, Math.max(0, wait) + 1000)
    return () => clearTimeout(timer)
  }, [data, refetch])

  const showNewKey = (title: string, rawKey: string, note?: string) => {
    setNewKey({ title, rawKey, note })
    void refetch()
  }

  const revoke = async () => {
    if (!revokeTarget || revoking) return
    setRevoking(true)
    try {
      await api.revokeApiKey(revokeTarget.id)
      toast.success('API key revoked')
    } catch (err) {
      if (!(err instanceof ApiError && err.status === 404)) {
        showApiError(err, 'Failed to revoke key')
        return
      }
      // Someone revoked it first, in another tab or session: the list was out
      // of date, not the request.
      toast.info('That key was already revoked.')
    } finally {
      setRevoking(false)
    }
    setRevokeTarget(null)
    void refetch()
  }
  const closeRevoke = (open: boolean) => { if (!open) setRevokeTarget(null) }

  const keys = data ?? []
  const activeKeys = keys.filter(k => k.status === 'active')
  const expiredKeys = keys.filter(k => k.status === 'expired')
  const revokedKeys = keys.filter(k => k.status === 'revoked')
  const hasInactive = expiredKeys.length + revokedKeys.length > 0
  // A key is never on a test clock, so its times are wall-clock.
  const now = wallClockNow()

  const keyRow = (k: APIKey) => {
    const active = k.status === 'active'
    const type = keyTypeLabel(k.key_type)
    return (
      <TableRow key={k.id} className={cn(ROW, !active && 'text-muted-foreground')}>
        <TableCell className={cn('whitespace-normal text-sm font-medium [overflow-wrap:anywhere]', active ? 'text-foreground' : 'text-muted-foreground')}>
          {k.name}
          {/* Below md the Type and Key columns are hidden, so both go here:
              after a rotation two keys have one name, and only the start of
              the key tells them apart. */}
          <span className="block text-xs font-normal text-muted-foreground md:hidden">
            {type} · <code className="font-mono">{k.key_prefix}…</code>
          </span>
        </TableCell>
        <TableCell className="hidden md:table-cell text-sm text-muted-foreground">{type}</TableCell>
        <TableCell className="hidden md:table-cell">
          <code className="text-xs font-mono text-muted-foreground">{k.key_prefix}…</code>
        </TableCell>
        <TableCell><StatusBadge status={keyStatus(k, tenantTZ, now)} /></TableCell>
        <TableCell className="hidden md:table-cell text-sm text-muted-foreground">
          {/* A key in its last week shows the hour too: after a rotation it is when the old key stops. */}
          {!k.expires_at ? 'Never' : expiringIn(k, tenantTZ, now) === undefined ? formatDate(k.expires_at, tenantTZ) : formatDateTime(k.expires_at, tenantTZ)}
        </TableCell>
        <TableCell className="hidden md:table-cell text-sm text-muted-foreground">{lastUsed(k, now)}</TableCell>
        <TableCell className="text-right">
          {/* Every active key can be revoked: a dashboard session is not tied
              to a key (ADR-011). */}
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
          <p className="text-sm text-muted-foreground mt-1">Keys your server uses to call the Hardcount API{activeKeys.length > 0 ? ` · ${activeKeys.length} active` : ''}</p>
        </div>
        <Button size="sm" onClick={() => setShowCreate(true)}>
          <Plus size={16} className="mr-2" />
          Create API key
        </Button>
      </div>

      <QuickStart />

      {keys.length > 0 && (
        <NoticeRow className="mt-4" icon={Shield} title="Keep secret keys on your server">
          A secret key has full access. Never put one in browser or mobile app code.
        </NoticeRow>
      )}

      {error ? (
        <Card className="mt-6">
          <CardContent className="p-0">
            {error instanceof OutOfDateError
              ? <LoadError what="API keys" error={error} onRetry={() => window.location.reload()} retryLabel="Reload" />
              : <LoadError what="API keys" error={error} onRetry={() => refetch()} />}
          </CardContent>
        </Card>
      ) : isLoading ? (
        <div className="mt-6">
          <CardListSkeleton rows={3} />
        </div>
      ) : keys.length === 0 ? (
        <Card className="mt-6">
          <CardContent className="p-0">
            <EmptyState
              icon={Key}
              title="No API keys yet"
              description="Create an API key to start using the Hardcount API from your server."
              action={{ label: 'Create API key', icon: Plus, onClick: () => setShowCreate(true) }}
            />
          </CardContent>
        </Card>
      ) : (
        <>
          <Card className="mt-6">
            <CardContent className="p-0">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead className={HEAD}>Name</TableHead>
                    <TableHead className={cn(HEAD, 'hidden md:table-cell')}>Type</TableHead>
                    <TableHead className={cn(HEAD, 'hidden md:table-cell')}>Key</TableHead>
                    <TableHead className={HEAD}>Status</TableHead>
                    <TableHead className={cn(HEAD, 'hidden md:table-cell')}>Expires</TableHead>
                    <TableHead className={cn(HEAD, 'hidden md:table-cell')}>Last used</TableHead>
                    <TableHead className={cn(HEAD, 'w-12')}><span className="sr-only">Actions</span></TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {activeKeys.length > 0 ? activeKeys.map(keyRow) : (
                    <TableRow className={ROW}>
                      <TableCell colSpan={7} className="whitespace-normal text-sm text-muted-foreground">
                        No active keys.{' '}
                        <Button variant="link" size="sm" className="h-auto p-0" onClick={() => setShowCreate(true)}>Create API key</Button>
                      </TableCell>
                    </TableRow>
                  )}
                </TableBody>
                {hasInactive && (
                  // Its own top border: the body above drops the border of its last row.
                  <TableBody id="api-key-inactive-rows" hidden={!showInactive} className="border-t">
                    {[...expiredKeys, ...revokedKeys].map(keyRow)}
                  </TableBody>
                )}
              </Table>
            </CardContent>
          </Card>

          {activeKeys.some(k => expiringIn(k, tenantTZ, now) !== undefined) && (
            <p className="mt-3 text-sm text-muted-foreground">
              A key cannot be extended. Before one expires, switch what uses it to another key.
            </p>
          )}

          {hasInactive && (
            <button onClick={() => setShowInactive(!showInactive)}
              aria-expanded={showInactive} aria-controls="api-key-inactive-rows"
              className="mt-3 flex items-center gap-2 text-sm text-muted-foreground hover:text-foreground transition-colors">
              <ChevronDown size={14} className={cn('transition-transform', showInactive && 'rotate-180')} />
              {showInactive ? 'Hide' : 'Show'} {inactiveLabel(expiredKeys.length, revokedKeys.length)}
            </button>
          )}
        </>
      )}

      {showCreate && (
        <CreateKeyDialog
          onClose={() => setShowCreate(false)}
          onCreated={(rawKey) => {
            setShowCreate(false)
            showNewKey('API key created', rawKey)
          }}
        />
      )}

      {rotateTarget && (
        <RotateKeyDialog
          target={rotateTarget}
          onClose={() => setRotateTarget(null)}
          onRotated={(rawKey, oldKey) => {
            setRotateTarget(null)
            showNewKey('API key rotated', rawKey, oldKeySentence(oldKey, tenantTZ))
          }}
        />
      )}

      {newKey && (
        <ShowOnceSecretDialog title={newKey.title} label="New key" secret={newKey.rawKey} onDone={() => setNewKey(null)}>
          {newKey.note && <p className="text-sm text-muted-foreground">{newKey.note}</p>}
          <TryIt rawKey={newKey.rawKey} />
        </ShowOnceSecretDialog>
      )}

      {/* A live key runs production integrations, and revoking it breaks them
          at once, so it takes the typed word. A test key gets the plain
          confirm. */}
      {revokeTarget && (revokeTarget.livemode ? (
        <TypedConfirmDialog
          open
          onOpenChange={closeRevoke}
          title="Revoke live API key"
          description={revokeSentence(revokeTarget)}
          confirmWord="REVOKE"
          confirmLabel="Revoke key"
          onConfirm={revoke}
          loading={revoking}
        />
      ) : (
        <AlertDialog open onOpenChange={closeRevoke}>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>Revoke test API key</AlertDialogTitle>
              <AlertDialogDescription>{revokeSentence(revokeTarget)}</AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel>Cancel</AlertDialogCancel>
              <AlertDialogAction onClick={revoke} disabled={revoking} className="bg-destructive text-destructive-foreground hover:bg-destructive/90">
                {revoking ? 'Revoking…' : 'Revoke key'}
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      ))}
    </>
  )
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

// The first call to make with a key: whoami names its account and mode.
function TryIt({ rawKey }: { rawKey: string }) {
  const curl = `curl ${API_ORIGIN}/v1/whoami -H "Authorization: Bearer ${rawKey}"`
  return (
    <div>
      <p className="text-xs font-medium text-muted-foreground">Try it</p>
      <div className="mt-1.5 flex items-start gap-2">
        <code className="flex-1 break-all rounded bg-muted px-2 py-1 font-mono text-xs text-foreground">{curl}</code>
        <CopyButton text={curl} name="Copy the try-it command" />
      </div>
      <p className="mt-1.5 text-xs text-muted-foreground">It answers with the account and mode the key belongs to.</p>
    </div>
  )
}
