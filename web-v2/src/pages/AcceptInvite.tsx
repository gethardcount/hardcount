import { useEffect, useState } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import { membersApi, type InvitePreview } from '@/lib/members'
import { authFormError } from '@/lib/authFormError'
import { useAuth } from '@/contexts/AuthContext'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { Loader2, Eye, EyeOff } from 'lucide-react'
import { VeloxLogo } from '@/components/VeloxLogo'

// AcceptInvitePage consumes a team-invite link (?token= planted by the
// invite email). Two shapes, decided by the server's preview:
//   - new account: set a password → account created, session minted,
//     straight into the dashboard.
//   - existing account: one-click accept → membership attached, then
//     off to /login to sign in with their own password (possession of
//     the email alone must not open an already-privileged account).
//
// Deliberately NOT wrapped in PublicOnlyRoute — the token is the proof
// of intent and may target a different account than any active session.
export default function AcceptInvitePage() {
  usePageTitle('Accept invitation')
  const [params] = useSearchParams()
  const token = params.get('token') ?? ''
  const navigate = useNavigate()
  const { refresh } = useAuth()

  const [preview, setPreview] = useState<InvitePreview | null>(null)
  const [tokenState, setTokenState] = useState<'checking' | 'valid' | 'invalid'>(token ? 'checking' : 'invalid')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [showPassword, setShowPassword] = useState(false)
  const [showConfirm, setShowConfirm] = useState(false)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const [accepted, setAccepted] = useState(false)

  useEffect(() => {
    if (!token) return
    let cancelled = false
    membersApi.previewInvite(token)
      .then(p => { if (!cancelled) { setPreview(p); setTokenState('valid') } })
      .catch(() => { if (!cancelled) setTokenState('invalid') })
    return () => { cancelled = true }
  }, [token])

  const handleAccept = async (e: React.FormEvent) => {
    e.preventDefault()
    setError('')
    if (preview?.needs_new_account) {
      if (password.length < 12) {
        setError('Password must be at least 12 characters')
        return
      }
      if (password !== confirm) {
        setError('Passwords do not match')
        return
      }
    }
    setLoading(true)
    try {
      const res = await membersApi.acceptInvite(token, password)
      if (res.session_minted) {
        // New account: the server set the session cookie — pull the
        // fresh identity into AuthContext, then land on the dashboard.
        await refresh()
        navigate('/', { replace: true })
        return
      }
      setAccepted(true)
    } catch (err) {
      setError(authFormError(err))
    } finally {
      setLoading(false)
    }
  }

  const workspace = preview?.tenant_name || 'the workspace'

  return (
    <div className="min-h-screen flex flex-col items-center justify-center px-4 bg-background">
      <div className="flex flex-col items-center mb-8">
        <VeloxLogo size="lg" />
        <p className="text-sm text-muted-foreground mt-2">
          {tokenState === 'valid' ? `You've been invited to join ${workspace}` : 'Team invitation'}
        </p>
      </div>

      <Card className="w-full max-w-[420px]">
        <CardContent className="p-6">
          {tokenState === 'checking' ? (
            <div className="flex items-center justify-center py-6 text-sm text-muted-foreground">
              <Loader2 size={16} className="animate-spin mr-2" /> Validating invitation…
            </div>
          ) : tokenState === 'invalid' ? (
            <div className="space-y-4 text-sm">
              <p className="text-foreground">
                This invitation link is no longer valid. It may have expired, been revoked,
                or already been used. Ask a workspace member to send a new invite.
              </p>
              <Link to="/login">
                <Button variant="outline" className="w-full">Go to sign in</Button>
              </Link>
            </div>
          ) : accepted ? (
            <div className="space-y-4 text-sm">
              <p className="text-foreground">
                You&apos;ve joined {workspace}. Sign in with your existing password to get started.
              </p>
              <Link to="/login">
                <Button className="w-full">Sign in</Button>
              </Link>
            </div>
          ) : (
            <form onSubmit={handleAccept} noValidate className="space-y-4">
              <div className="text-sm text-muted-foreground">
                {preview?.invited_by_email && (
                  <p><span className="text-foreground">{preview.invited_by_email}</span> invited</p>
                )}
                <p className="text-foreground font-medium">{preview?.email}</p>
              </div>

              {preview?.needs_new_account ? (
                <>
                  <div className="space-y-1.5">
                    <Label htmlFor="password">Choose a password</Label>
                    <div className="relative">
                      <Input
                        id="password"
                        type={showPassword ? 'text' : 'password'}
                        value={password}
                        onChange={e => setPassword(e.target.value)}
                        placeholder="At least 12 characters"
                        autoComplete="new-password"
                        autoFocus
                        className="pr-10"
                      />
                      <button
                        type="button"
                        onClick={() => setShowPassword(s => !s)}
                        aria-label={showPassword ? 'Hide password' : 'Show password'}
                        className="absolute right-2 top-1/2 -translate-y-1/2 p-1 rounded text-muted-foreground hover:text-foreground hover:bg-accent transition-colors"
                        tabIndex={-1}
                      >
                        {showPassword ? <EyeOff size={16} /> : <Eye size={16} />}
                      </button>
                    </div>
                  </div>

                  <div className="space-y-1.5">
                    <Label htmlFor="confirm">Confirm password</Label>
                    <div className="relative">
                      <Input
                        id="confirm"
                        type={showConfirm ? 'text' : 'password'}
                        value={confirm}
                        onChange={e => setConfirm(e.target.value)}
                        placeholder="Re-enter your password"
                        autoComplete="new-password"
                        className="pr-10"
                      />
                      <button
                        type="button"
                        onClick={() => setShowConfirm(s => !s)}
                        aria-label={showConfirm ? 'Hide password' : 'Show password'}
                        className="absolute right-2 top-1/2 -translate-y-1/2 p-1 rounded text-muted-foreground hover:text-foreground hover:bg-accent transition-colors"
                        tabIndex={-1}
                      >
                        {showConfirm ? <EyeOff size={16} /> : <Eye size={16} />}
                      </button>
                    </div>
                  </div>
                </>
              ) : (
                <p className="text-sm text-muted-foreground">
                  You already have a Velox account with this email. Accepting adds {workspace} to it.
                </p>
              )}

              {error && (
                <div className="px-3 py-2.5 rounded-lg bg-destructive/10 border border-destructive/20">
                  <p className="text-destructive text-sm">{error}</p>
                </div>
              )}

              <Button type="submit" disabled={loading} className="w-full">
                {loading ? <Loader2 size={16} className="animate-spin mr-2" /> : null}
                {loading
                  ? 'Joining…'
                  : preview?.needs_new_account ? 'Create account & join' : 'Accept invitation'}
              </Button>
            </form>
          )}
        </CardContent>
      </Card>
    </div>
  )
}
