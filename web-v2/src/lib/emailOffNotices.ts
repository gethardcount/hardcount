// The two places where the dashboard depends on the server being able to send
// email links: Settings → Team invites and Forgot password. Each has two
// versions, email set up or not; both versions live here so a test covers
// each one.
//
// Dependency-free so node --test can load it.

import type { CalloutTone } from './calloutTone'

export interface EmailNotice {
  tone: CalloutTone
  title: string
  body: string
  // The off version carries a Details line for whoever runs Velox (env var
  // names live there, never in the sentence).
  operatorDetails: boolean
}

// Team tab. can_send_invites is the server's own check (dashmembers
// CanInvite: DASHBOARD_BASE_URL set and email configured); false is the only
// case where Send invite would be refused. Undefined while whoami loads: the
// form stays on rather than flashing a warning.
export function inviteFormState(canSendInvites: boolean | undefined): {
  off: boolean
  notice: EmailNotice | null
} {
  if (canSendInvites !== false) return { off: false, notice: null }
  return {
    off: true,
    notice: {
      tone: 'amber',
      title: 'Invites are off',
      body: "This server isn't set up to email invite links. Ask whoever runs Velox to set up email.",
      operatorDetails: true,
    },
  }
}

// What the Forgot password page shows after submit. not_configured means SMTP
// or the dashboard address is missing (user handler), so "can't send email"
// would be untrue when only the address is; the sentence says "reset links".
//
// The other version stays conditional on purpose: the reply is the same
// whether or not the address has an account, and whether or not the
// 3-per-hour cap skipped the send (internal/user/handler.go), so it can't be
// used to find out who has an account.
export function resetSentNotice(emailDelivery: string | undefined, email: string): EmailNotice {
  if (emailDelivery === 'not_configured') {
    return {
      tone: 'amber',
      title: 'Password reset email is off',
      body: "This server isn't set up to send reset links, so none was sent. Ask whoever runs Velox to set up email.",
      operatorDetails: true,
    }
  }
  return {
    tone: 'neutral',
    title: 'Check your email',
    body: `If an account exists for ${email}, a reset link is on its way. It works for 1 hour, and we send at most 3 an hour.`,
    operatorDetails: false,
  }
}
