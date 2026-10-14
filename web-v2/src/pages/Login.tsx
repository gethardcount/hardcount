import { useState } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { Link, useLocation, useNavigate, useSearchParams } from 'react-router-dom'
import { returnPath } from '@/lib/returnPath'
import { SESSION_ENDED_MESSAGE } from '@/lib/errorCopy'
import { Callout } from '@/components/Callout'
import { useAuth } from '@/contexts/AuthContext'
import { ApiError } from '@/lib/api'
import { SessionNotKeptError, sessionNotKeptMessage, signInRefusedMessage } from '@/lib/sessionKept'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { Loader2, Eye, EyeOff } from 'lucide-react'
import { VeloxLogo } from '@/components/VeloxLogo'

export default function LoginPage() {
  usePageTitle('Sign in')
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [showPassword, setShowPassword] = useState(false)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const navigate = useNavigate()
  const location = useLocation()
  const { login, sessionEnded } = useAuth()
  // ?reset=success is set by ResetPassword on a clean update so the
  // operator sees confirmation that the new password took effect.
  // Cleared as soon as they start typing to avoid a stale banner.
  const [searchParams] = useSearchParams()
  const showResetSuccess = searchParams.get('reset') === 'success' && !email && !error

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError('')

    if (!email.trim() || !password) {
      setError('Email and password are required')
      return
    }

    setLoading(true)
    try {
      await login(email.trim(), password)
      navigate(returnPath(location.state), { replace: true })
    } catch (err) {
      if (err instanceof SessionNotKeptError) {
        setError(sessionNotKeptMessage(window.isSecureContext))
      } else if (err instanceof ApiError) {
        if (err.status === 401) {
          setError('Invalid email or password')
        } else if (err.status === 429) {
          // 429 comes only from the per-IP rate limiter (a ~1-minute window),
          // never from account lockout — that is deliberately masked as 401 for
          // anti-enumeration (user/handler.go). So this copy must NOT promise a
          // 15-minute lockout wait; it describes the rate limit, with no number.
          setError('Too many attempts. Please wait a moment and try again.')
        } else if (err.status === 403) {
          setError(signInRefusedMessage(window.isSecureContext, err.message))
        } else {
          setError(err.message)
        }
      } else {
        setError('Cannot connect to Velox API')
      }
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="min-h-screen flex flex-col items-center justify-center px-4 bg-background">
      <div className="flex flex-col items-center mb-8">
        <VeloxLogo size="lg" />
        <p className="text-sm text-muted-foreground mt-2">Sign in to the dashboard</p>
      </div>

      <Card className="w-full max-w-[420px]">
        <CardContent className="p-6">
          {showResetSuccess && (
            <div className="mb-4 px-3 py-2.5 rounded-lg bg-emerald-500/10 border border-emerald-500/30">
              <p className="text-emerald-700 dark:text-emerald-400 text-sm">
                Password updated. Sign in with your new password.
              </p>
            </div>
          )}
          {/* The session ended while a page was open (lib/sessionEnd). After
              sign-in the operator returns to that page. Hidden once an error
              shows, so only one message is on screen. */}
          {sessionEnded && !error && !showResetSuccess && (
            <Callout className="mb-4">{SESSION_ENDED_MESSAGE}</Callout>
          )}
          <form onSubmit={handleSubmit} noValidate className="space-y-4">
            <div className="space-y-1.5">
              <Label htmlFor="email">Email</Label>
              <Input
                id="email"
                type="email"
                value={email}
                onChange={e => setEmail(e.target.value)}
                placeholder="you@example.com"
                autoComplete="email"
                autoFocus
                spellCheck={false}
              />
            </div>

            <div className="space-y-1.5">
              <div className="flex items-center justify-between">
                <Label htmlFor="password">Password</Label>
                <Link
                  to="/forgot-password"
                  className="text-xs text-muted-foreground hover:text-foreground transition-colors"
                >
                  Forgot password?
                </Link>
              </div>
              <div className="relative">
                <Input
                  id="password"
                  type={showPassword ? 'text' : 'password'}
                  value={password}
                  onChange={e => setPassword(e.target.value)}
                  placeholder="Enter your password"
                  autoComplete="current-password"
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

            {error && (
              <div className="px-3 py-2.5 rounded-lg bg-destructive/10 border border-destructive/20">
                <p className="text-destructive text-sm">{error}</p>
              </div>
            )}

            <Button type="submit" disabled={loading} className="w-full">
              {loading ? <Loader2 size={16} className="animate-spin mr-2" /> : null}
              {loading ? 'Signing in…' : 'Sign In'}
            </Button>
          </form>
        </CardContent>
      </Card>
      <p className="text-xs text-muted-foreground mt-6">
        Need help?{' '}
        <a
          href="https://github.com/getvelox/velox/issues"
          target="_blank"
          rel="noopener noreferrer"
          className="text-foreground hover:underline"
        >
          Open an issue on GitHub
        </a>
      </p>
    </div>
  )
}
