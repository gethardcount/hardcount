import { useState } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { Link } from 'react-router-dom'
import { authApi } from '@/lib/auth'
import { authFormError } from '@/lib/authFormError'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { Loader2 } from 'lucide-react'
import { VeloxLogo } from '@/components/VeloxLogo'
import { Callout } from '@/components/Callout'

// ForgotPasswordPage is the dashboard's password-reset request flow.
// Always renders the same "if your email is on file, a reset link
// has been sent" message regardless of whether the email matched a
// user — defends against account enumeration via response timing or
// content. ADR-011.
//
// When email delivery is not configured on the server (SMTP unset or
// DASHBOARD_BASE_URL unset), the response carries email_delivery:
// 'not_configured' and we tell the operator no link can be sent — there
// is NO stdout/log fallback for the link (ADR-011 removed it).
export default function ForgotPasswordPage() {
  usePageTitle('Reset password')
  const [email, setEmail] = useState('')
  const [submitted, setSubmitted] = useState(false)
  const [emailNotConfigured, setEmailNotConfigured] = useState(false)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError('')
    if (!email.trim()) {
      setError('Email is required')
      return
    }
    setLoading(true)
    try {
      const res = await authApi.requestPasswordReset(email.trim())
      setEmailNotConfigured(res?.email_delivery === 'not_configured')
      setSubmitted(true)
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
        <p className="text-sm text-muted-foreground mt-2">Reset your password</p>
      </div>

      <Card className="w-full max-w-[420px]">
        <CardContent className="p-6">
          {submitted ? (
            <div className="space-y-4 text-sm">
              {emailNotConfigured ? (
                <Callout
                  tone="amber"
                  title="Password reset email is off"
                  details={
                    <p>
                      For whoever runs Velox: set <code className="font-mono">SMTP_HOST</code> and{' '}
                      <code className="font-mono">DASHBOARD_BASE_URL</code> (docs/ops/email-setup.md).
                    </p>
                  }
                >
                  {/* not_configured: SMTP or the dashboard address is missing, so "can't send email" would be untrue when only the address is. */}
                  <p>This server isn&rsquo;t set up to send reset links, so none was sent. Ask whoever runs Velox to set up email.</p>
                </Callout>
              ) : (
                // Stays conditional on purpose: the reply is the same whether
                // or not the address has an account, and whether or not the
                // 3-per-hour cap skipped the send (internal/user/handler.go),
                // so it can't be used to find out who has an account.
                <Callout title="Check your email">
                  <p>
                    If an account exists for <strong>{email}</strong>, a reset link is on its way. It works for
                    1 hour, and we send at most 3 an hour.
                  </p>
                </Callout>
              )}
              <Link to="/login">
                <Button variant="outline" className="w-full">
                  Back to sign in
                </Button>
              </Link>
            </div>
          ) : (
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
                <p className="text-xs text-muted-foreground">
                  We&rsquo;ll send a reset link to this email.
                </p>
              </div>

              {error && (
                <div className="px-3 py-2.5 rounded-lg bg-destructive/10 border border-destructive/20">
                  <p className="text-destructive text-sm">{error}</p>
                </div>
              )}

              <Button type="submit" disabled={loading} className="w-full">
                {loading ? <Loader2 size={16} className="animate-spin mr-2" /> : null}
                {loading ? 'Sending…' : 'Send reset link'}
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
