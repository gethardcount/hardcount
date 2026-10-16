// Settings → Email: what this server can send, in plain words. Plain
// functions so node --test can check every state.
//
// Email is configured in the server environment, not in the dashboard. The
// status comes from GET /v1/settings/email; nothing here is editable.

export interface EmailLinkEnv {
  env: string
  set: boolean
  effect: string
}

export interface EmailSettingsStatus {
  smtp_configured: boolean
  smtp_host: string
  from_address: string
  tls_mode: string
  links: EmailLinkEnv[]
  can_email_setup_links: boolean
  can_send_invites: boolean
}

export type EmailSetupState = 'ready' | 'partial' | 'off'

export interface EmailSetupSummary {
  state: EmailSetupState
  // Short status word for the section badge.
  label: string
  title: string
  body: string
  missingLinks: EmailLinkEnv[]
}

// emailSetupSummary: off = no SMTP, nothing is sent; partial = SMTP works
// but some emails go out without a link or can't be sent; ready = all set.
export function emailSetupSummary(s: EmailSettingsStatus): EmailSetupSummary {
  const missingLinks = (s.links ?? []).filter(l => !l.set)
  if (!s.smtp_configured) {
    return {
      state: 'off',
      label: 'Not set up',
      title: "This server can't send email",
      body: 'Invoices, receipts, payment reminders, password resets and team invites all fail until SMTP_HOST is set on the server.',
      missingLinks,
    }
  }
  if (missingLinks.length > 0) {
    return {
      state: 'partial',
      label: 'Partly set up',
      title: missingLinks.length === 1 ? 'One email link is not set up' : `${missingLinks.length} email links are not set up`,
      body: 'Email is sent, but the emails listed below go out without their link or are not sent.',
      missingLinks,
    }
  }
  return { state: 'ready', label: 'Ready', title: 'Email is set up', body: 'Every email type can be sent with its links.', missingLinks }
}

const TLS_LABELS: Record<string, string> = {
  starttls: 'STARTTLS (port 587)',
  implicit: 'TLS (port 465)',
  none: 'No encryption (local only)',
}

export function tlsModeLabel(mode: string): string {
  return TLS_LABELS[mode] ?? mode
}

// testEmailOutcome turns the test-send result into the line shown under the
// button. code is the API error code when the send failed.
export function testEmailOutcome(
  result: { sentTo: string } | { code?: string; message: string },
): { tone: 'success' | 'danger'; text: string } {
  if ('sentTo' in result) {
    return { tone: 'success', text: `Sent to ${result.sentTo}. If it doesn't arrive in a few minutes, check spam and the server log.` }
  }
  if (result.code === 'email_not_configured') {
    return { tone: 'danger', text: "Not sent: this server has no SMTP server set up." }
  }
  return { tone: 'danger', text: result.message || 'Not sent: the email server did not accept it.' }
}
