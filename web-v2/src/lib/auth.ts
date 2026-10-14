// Dashboard auth: the operator signs in with email + password; the
// backend's POST /v1/auth/login validates the credentials and mints
// an httpOnly session cookie. Subsequent requests ride the cookie via
// `credentials: 'include'`. The password never touches localStorage
// or any other JS-readable storage — only the login form field for
// the duration of the submit. See ADR-011.
//
// API keys are now SDK/curl-only credentials; they don't sign in to
// the dashboard.

import { apiRequest } from './api'

export interface SessionInfo {
  tenant_id: string
  // Either user_id (cookie path, post-ADR-011) or key_id (Bearer
  // path) is set, never both — depends on which middleware
  // resolved the request. The dashboard cookie path always
  // populates user_id; SDK Bearer callers see key_id.
  user_id?: string
  key_id?: string
  key_type?: string
  email?: string
  livemode: boolean
  // Whether this server can email a customer a setup link (SMTP and
  // PAYMENT_UPDATE_URL both set). Server config, not per user.
  can_email_setup_links?: boolean
  // Whether this server can invite a teammate (email sending and its
  // dashboard address are both set up). Server config, not per user.
  can_send_invites?: boolean
}

export interface LoginResponse {
  user_id: string
  tenant_id: string
  email: string
  livemode: boolean
  expires_at: string
}

export const authApi = {
  // login takes email + password, validates server-side, and sets the
  // httpOnly session cookie on the response.
  login: (email: string, password: string) =>
    apiRequest<LoginResponse>('POST', '/auth/login', { email, password }),

  // logout revokes the server-side session row and clears the cookie.
  logout: () => apiRequest<void>('POST', '/auth/logout'),

  // whoami resolves the current session (or Bearer key, for SDK
  // callers) to its tenant context. Throws ApiError(401) when neither
  // credential is present or both are stale.
  whoami: () => apiRequest<SessionInfo>('GET', '/whoami'),

  // requestPasswordReset triggers the "send a reset link" flow.
  // Always 200; never confirms whether the email was on file.
  // email_delivery reflects server config (SMTP + DASHBOARD_BASE_URL),
  // not whether the email matched — used to warn self-hosters whose
  // email isn't wired that no link can actually arrive.
  requestPasswordReset: (email: string) =>
    apiRequest<{ message: string; email_delivery?: 'ok' | 'not_configured' }>('POST', '/auth/password-reset/request', { email }),

  // confirmPasswordReset consumes a reset token and sets a new
  // password. Token from the email link's ?token= param.
  confirmPasswordReset: (token: string, password: string) =>
    apiRequest<{ message: string }>('POST', '/auth/password-reset/confirm', { token, password }),

  // checkPasswordResetToken validates a token without consuming it.
  // The reset-password page calls this on mount so it can render
  // "link no longer valid" instead of a form the user fills in only
  // to be rejected at submit. Throws ApiError(422) when invalid.
  checkPasswordResetToken: (token: string) =>
    apiRequest<{ valid: boolean }>('GET', `/auth/password-reset/check?token=${encodeURIComponent(token)}`),

  // setMode flips the active mode (test/live) on the cookie session.
  // All subsequent requests inherit the new mode automatically.
  setMode: (livemode: boolean) =>
    apiRequest<{ livemode: boolean }>('POST', '/auth/mode', { livemode }),
}
