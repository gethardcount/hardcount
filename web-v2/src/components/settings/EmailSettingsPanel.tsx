import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Check, Loader2, Mail, X } from 'lucide-react'

import { ApiError, api } from '@/lib/api'
import { emailSetupSummary, testEmailOutcome, tlsModeLabel } from '@/lib/emailSettings'
import { useAuth } from '@/contexts/AuthContext'
import { Section } from '@/components/page/Section'
import { PropertyList } from '@/components/page/PropertyList'
import { Callout } from '@/components/Callout'
import { LoadError } from '@/components/LoadError'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'

// Settings → Email. Read-only: email is configured on the server (SMTP_* and
// the link base URLs), so this page says what works, what doesn't and why,
// and offers a test send to your own address.
export function EmailSettingsPanel() {
  const { user } = useAuth()
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ['email-settings'],
    queryFn: () => api.getEmailSettings(),
  })
  const [sending, setSending] = useState(false)
  const [outcome, setOutcome] = useState<ReturnType<typeof testEmailOutcome> | null>(null)

  if (isLoading) {
    return <div className="flex justify-center p-8"><Loader2 className="h-6 w-6 animate-spin text-muted-foreground" /></div>
  }
  if (error || !data) return <LoadError what="email settings" error={error} onRetry={() => refetch()} />

  const summary = emailSetupSummary(data)

  const sendTest = async () => {
    setSending(true)
    setOutcome(null)
    try {
      const res = await api.sendTestEmail()
      setOutcome(testEmailOutcome({ sentTo: res.sent_to }))
    } catch (err) {
      const e = err as ApiError
      setOutcome(testEmailOutcome({ code: e?.code, message: e?.message ?? '' }))
    } finally {
      setSending(false)
    }
  }

  return (
    <div className="space-y-6">
      <Section
        title="Email delivery"
        description="Set on the server with the SMTP_* environment variables. Changes need a restart."
        actions={
          <Badge variant={summary.state === 'ready' ? 'success' : summary.state === 'partial' ? 'warning' : 'danger'}>
            {summary.label}
          </Badge>
        }
      >
        {summary.state !== 'ready' && (
          <Callout tone={summary.state === 'off' ? 'red' : 'amber'} title={summary.title} className="mb-4"
            details={<p>See <span className="font-mono">docs/ops/email-setup.md</span> for each variable.</p>}>
            {summary.body}
          </Callout>
        )}
        <PropertyList
          className="grid grid-cols-1 gap-x-6 gap-y-3 space-y-0 sm:grid-cols-3"
          items={[
            { label: 'SMTP server', value: data.smtp_host || 'Not set', mono: !!data.smtp_host },
            { label: 'From address', value: data.from_address, mono: true },
            { label: 'Encryption', value: tlsModeLabel(data.tls_mode) },
          ]}
        />
        <div className="mt-5 flex flex-wrap items-center gap-3 border-t border-border pt-4">
          <Button variant="outline" size="sm" onClick={sendTest} disabled={!data.smtp_configured || sending}>
            {sending ? <Loader2 size={14} className="mr-1.5 animate-spin" /> : <Mail size={14} className="mr-1.5" />}
            Send test email
          </Button>
          <span className="text-sm text-muted-foreground">
            {data.smtp_configured
              ? <>Goes to your address{user?.email ? <>, <span className="text-foreground">{user.email}</span></> : ''}.</>
              : 'Available once SMTP_HOST is set.'}
          </span>
        </div>
        {outcome && (
          <p role="status" className={cn('mt-3 text-sm', outcome.tone === 'success' ? 'text-success-foreground' : 'text-danger-foreground')}>
            {outcome.text}
          </p>
        )}
      </Section>

      <Section title="Email links" description="Links inside emails are built from these server addresses.">
        <ul className="divide-y divide-border">
          {data.links.map(l => (
            <li key={l.env} className="flex items-start gap-3 py-3 first:pt-0 last:pb-0">
              {l.set
                ? <Check size={16} aria-hidden className="mt-0.5 shrink-0 text-success-foreground" />
                : <X size={16} aria-hidden className="mt-0.5 shrink-0 text-warning-foreground" />}
              <div className="min-w-0 flex-1">
                <p className="font-mono text-xs text-foreground">{l.env}</p>
                <p className="mt-0.5 text-sm text-muted-foreground">{l.set ? 'Set.' : `Not set. ${l.effect}`}</p>
              </div>
              <Badge variant={l.set ? 'success' : 'warning'}>{l.set ? 'Set' : 'Not set'}</Badge>
            </li>
          ))}
        </ul>
      </Section>
    </div>
  )
}
