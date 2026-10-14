import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { Loader2, Mail, UserPlus, Users } from 'lucide-react'
import { membersApi, type InvitationView, type MemberView } from '@/lib/members'
import { formatDate } from '@/lib/api'
import { authApi } from '@/lib/auth'
import { Callout } from '@/components/Callout'
import { showApiError } from '@/lib/formErrors'
import { inviteFormState } from '@/lib/emailOffNotices'
import { useAuth } from '@/contexts/AuthContext'
import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { LoadError } from '@/components/LoadError'
import { Input } from '@/components/ui/input'
import { Badge } from '@/components/ui/badge'
import { CardListSkeleton } from '@/components/ui/TableSkeleton'
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

// TeamTab is the Settings → Team surface: who can sign in to this
// workspace, plus pending invitations. Minimal by design — no RBAC yet;
// every member has full access. Roles show as informational badges only.

function statusVariant(status: InvitationView['status']): 'default' | 'secondary' | 'outline' | 'destructive' {
  switch (status) {
    case 'pending': return 'default'
    case 'expired': return 'outline'
    case 'revoked': return 'destructive'
    default: return 'secondary'
  }
}

export function TeamTab() {
  const { user } = useAuth()
  const queryClient = useQueryClient()
  const [inviteEmail, setInviteEmail] = useState('')
  const [inviting, setInviting] = useState(false)
  const [removeTarget, setRemoveTarget] = useState<MemberView | null>(null)
  const [removing, setRemoving] = useState(false)

  // can_send_invites is the server's own check (dashmembers CanInvite):
  // false when email or the dashboard address isn't set up, the two cases
  // where Send invite would be refused. Unknown while loading: form stays on.
  const { data: whoami } = useQuery({
    queryKey: ['whoami'],
    queryFn: () => authApi.whoami(),
  })
  const { off: invitesOff, notice: invitesNotice } = inviteFormState(whoami?.can_send_invites)

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ['members'],
    queryFn: () => membersApi.list(),
  })

  const members = data?.members ?? []
  const invitations = data?.invitations ?? []
  // Accepted invites already show up as members; showing the historical
  // rows too would double-list people, so keep pending/expired/revoked.
  const openInvitations = invitations.filter(i => i.status !== 'accepted')

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ['members'] })

  const handleInvite = async (e: React.FormEvent) => {
    e.preventDefault()
    const email = inviteEmail.trim()
    if (!email || inviting) return
    setInviting(true)
    try {
      await membersApi.invite(email)
      // Not "sent": the server queues the email (outbox) and refuses
      // outright when email isn't set up (email_not_configured), so
      // "on its way" is what a 201 proves. InviteTTL is 7 days.
      toast.success(`Invitation created for ${email}`, {
        description: "We're emailing them a link to join. It works for 7 days.",
      })
      setInviteEmail('')
      invalidate()
    } catch (err) {
      showApiError(err, 'Failed to send invitation')
    } finally {
      setInviting(false)
    }
  }

  const handleRevoke = async (inv: InvitationView) => {
    try {
      await membersApi.revokeInvitation(inv.id)
      toast.success(`Invitation to ${inv.email} revoked`)
      invalidate()
    } catch (err) {
      showApiError(err, 'Failed to revoke invitation')
    }
  }

  const handleRemove = async () => {
    if (!removeTarget || removing) return
    setRemoving(true)
    try {
      await membersApi.removeMember(removeTarget.user_id)
      toast.success(`${removeTarget.email} removed from the workspace`)
      setRemoveTarget(null)
      invalidate()
    } catch (err) {
      showApiError(err, 'Failed to remove member')
    } finally {
      setRemoving(false)
    }
  }

  return (
    <div className="space-y-6">
      {/* Invite form */}
      <Card>
        <CardContent className="px-6 py-5">
          <div className="flex items-center gap-2 mb-1">
            <UserPlus size={15} className="text-muted-foreground" />
            <h3 className="text-sm font-medium text-foreground">Invite a teammate</h3>
          </div>
          <p className="text-xs text-muted-foreground mb-4">
            They&apos;ll get an email with a link to join this workspace, valid for 7 days. Every
            member has full access.
          </p>
          {invitesNotice && (
            <Callout
              tone={invitesNotice.tone}
              title={invitesNotice.title}
              className="mb-4"
              details={
                <p>
                  For whoever runs Velox: set <code className="font-mono">SMTP_HOST</code> and{' '}
                  <code className="font-mono">DASHBOARD_BASE_URL</code> (docs/ops/email-setup.md).
                </p>
              }
            >
              <p>{invitesNotice.body}</p>
            </Callout>
          )}
          <form onSubmit={handleInvite} className="flex gap-2 max-w-md">
            <Input
              type="email"
              value={inviteEmail}
              onChange={e => setInviteEmail(e.target.value)}
              placeholder="teammate@company.com"
              aria-label="Email to invite"
              disabled={invitesOff}
            />
            <Button type="submit" disabled={invitesOff || inviting || !inviteEmail.trim()}>
              {inviting ? <Loader2 size={14} className="animate-spin mr-1.5" /> : <Mail size={14} className="mr-1.5" />}
              {inviting ? 'Sending…' : 'Send invite'}
            </Button>
          </form>
        </CardContent>
      </Card>

      {error ? (
        <Card>
          <CardContent className="p-0">
            <LoadError what="team members" error={error} onRetry={() => refetch()} />
          </CardContent>
        </Card>
      ) : isLoading ? (
        <CardListSkeleton rows={2} />
      ) : (
        <>
          {/* Members */}
          <Card>
            <CardContent className="px-6 py-5">
              <div className="flex items-center gap-2 mb-4">
                <Users size={15} className="text-muted-foreground" />
                <h3 className="text-sm font-medium text-foreground">
                  Members <span className="text-muted-foreground font-normal">({members.length})</span>
                </h3>
              </div>
              <div className="divide-y divide-border">
                {members.map(m => (
                  <div key={m.user_id} className="flex items-center justify-between py-3 first:pt-0 last:pb-0">
                    <div>
                      <div className="flex items-center gap-2">
                        <p className="text-sm text-foreground">{m.email}</p>
                        {m.user_id === user?.user_id && <Badge variant="outline">You</Badge>}
                        <Badge variant="secondary">{m.role}</Badge>
                      </div>
                      <p className="text-xs text-muted-foreground mt-0.5">Joined {formatDate(m.joined_at)}</p>
                    </div>
                    {m.user_id !== user?.user_id && (
                      <Button
                        variant="outline"
                        size="sm"
                        className="text-destructive hover:text-destructive"
                        onClick={() => setRemoveTarget(m)}
                      >
                        Remove
                      </Button>
                    )}
                  </div>
                ))}
              </div>
            </CardContent>
          </Card>

          {/* Invitations */}
          {openInvitations.length > 0 && (
            <Card>
              <CardContent className="px-6 py-5">
                <h3 className="text-sm font-medium text-foreground mb-4">Invitations</h3>
                <div className="divide-y divide-border">
                  {openInvitations.map(inv => (
                    <div key={inv.id} className="flex items-center justify-between py-3 first:pt-0 last:pb-0">
                      <div>
                        <div className="flex items-center gap-2">
                          <p className="text-sm text-foreground">{inv.email}</p>
                          <Badge variant={statusVariant(inv.status)}>{inv.status}</Badge>
                        </div>
                        <p className="text-xs text-muted-foreground mt-0.5">
                          {inv.invited_by_email ? `Invited by ${inv.invited_by_email} · ` : ''}
                          {inv.status === 'pending'
                            ? `Expires ${formatDate(inv.expires_at)}`
                            : `Sent ${formatDate(inv.created_at)}`}
                        </p>
                      </div>
                      {inv.status === 'pending' && (
                        <Button
                          variant="outline"
                          size="sm"
                          className="text-destructive hover:text-destructive"
                          onClick={() => handleRevoke(inv)}
                        >
                          Revoke
                        </Button>
                      )}
                    </div>
                  ))}
                </div>
              </CardContent>
            </Card>
          )}
        </>
      )}

      <AlertDialog open={!!removeTarget} onOpenChange={(open) => { if (!open) setRemoveTarget(null) }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Remove member</AlertDialogTitle>
            <AlertDialogDescription>
              {removeTarget
                ? `Remove ${removeTarget.email} from this workspace? They'll be signed out everywhere and lose access immediately. You can invite them back later.`
                : ''}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel onClick={() => setRemoveTarget(null)}>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={handleRemove}
              disabled={removing}
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
            >
              {removing ? 'Removing…' : 'Remove member'}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}
