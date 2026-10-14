import { useEffect, useState } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import { authApi } from '@/lib/auth'
import { authFormError } from '@/lib/authFormError'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { Loader2, Eye, EyeOff } from 'lucide-react'
import { VeloxLogo } from '@/components/VeloxLogo'

// ResetPasswordPage is the dashboard's confirm step for password
// reset. Reads the one-time token from ?token= in the URL (planted
// there by the email link), takes the new password, posts to
// /v1/auth/password-reset/confirm. Single-use token; consuming it
// invalidates it server-side. ADR-011.
//
// Password validation surface (length minimum, common-password
// rejection) lives server-side in internal/user.ValidatePassword;
// errors come back via 422 with field=password and bubble up
// inline.
export default function ResetPasswordPage() {
  usePageTitle('Reset password')
  const [params] = useSearchParams()
  const token = params.get('token') ?? ''
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [showPassword, setShowPassword] = useState(false)
  const [showConfirm, setShowConfirm] = useState(false)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  // tokenState: 'checking' (initial probe in flight), 'valid' (form
  // renderable), 'invalid' (already-used / expired / unknown — show
  // "link no longer valid" + a re-request CTA).
  const [tokenState, setTokenState] = useState<'checking' | 'valid' | 'invalid'>(token ? 'checking' : 'invalid')
  const navigate = useNavigate()

  useEffect(() => {
    if (!token) return
    let cancelled = false
    authApi.checkPasswordResetToken(token)
      .then(() => { if (!cancelled) setTokenState('valid') })
      .catch(() => { if (!cancelled) setTokenState('invalid') })
    return () => { cancelled = true }
  }, [token])

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError('')
    if (!token) {
      setError('This reset link is incomplete. Request a new one from the Forgot password page.')
      return
    }
    if (password.length < 12) {
      setError('Password must be at least 12 characters')
      return
    }
    if (password !== confirm) {
      setError('Passwords do not match')
      return
    }
    setLoading(true)
    try {
      await authApi.confirmPasswordReset(token, password)
      navigate('/login?reset=success', { replace: true })
    } catch (err) {
      setError(authFormError(err))
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="min-h-screen flex flex-col items-center justify-center px-4 bg-background">
      <div className="flex flex-col items-center mb-8">
        <VeloxLogo size="lg" />
        <p className="text-sm text-muted-foreground mt-2">Choose a new password</p>
      </div>

      <Card className="w-full max-w-[420px]">
        <CardContent className="p-6">
          {tokenState === 'checking' ? (
            <div className="flex items-center justify-center py-6 text-sm text-muted-foreground">
              <Loader2 size={16} className="animate-spin mr-2" /> Validating link…
            </div>
          ) : tokenState === 'invalid' ? (
            <div className="space-y-4 text-sm">
              <p className="text-foreground">
                This reset link is no longer valid. It may have been used already or expired.
              </p>
              <Link to="/forgot-password">
                <Button variant="outline" className="w-full">Request a new reset link</Button>
              </Link>
            </div>
          ) : (
            <form onSubmit={handleSubmit} noValidate className="space-y-4">
              <div className="space-y-1.5">
                <Label htmlFor="password">New password</Label>
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

              {error && (
                <div className="px-3 py-2.5 rounded-lg bg-destructive/10 border border-destructive/20">
                  <p className="text-destructive text-sm">{error}</p>
                </div>
              )}

              <Button type="submit" disabled={loading} className="w-full">
                {loading ? <Loader2 size={16} className="animate-spin mr-2" /> : null}
                {loading ? 'Updating…' : 'Update password'}
              </Button>

              <Link to="/login" className="block text-center text-xs text-muted-foreground hover:text-foreground transition-colors">
                Back to sign in
              </Link>
            </form>
          )}
        </CardContent>
      </Card>
    </div>
  )
}
