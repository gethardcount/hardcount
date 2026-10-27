import { apiRequest } from './api'

// Mirrors MemberView in internal/dashmembers/handler.go.
export interface MemberView {
  user_id: string
  email: string
  role: string
  joined_at: string
}

// Mirrors invitationView in internal/dashmembers/handler.go. `status` is
// derived server-side from accepted_at/revoked_at/expires_at; the client
// should treat it as authoritative rather than re-deriving.
export interface InvitationView {
  id: string
  email: string
  role: string
  status: 'pending' | 'accepted' | 'revoked' | 'expired'
  invited_by_email?: string
  expires_at: string
  created_at: string
  accepted_at?: string
  revoked_at?: string
}

export interface MembersListResp {
  members: MemberView[]
  invitations: InvitationView[]
}

// InvitePreview drives the accept-invite screen's copy (new-account vs
// existing-account) and the workspace name header.
export interface InvitePreview {
  email: string
  tenant_id: string
  tenant_name: string
  needs_new_account: boolean
  invited_by_email?: string
  expires_at: string
}

// session_minted is true only on the new-account path — the server
// auto-logs the new user in (they just set their password). Existing
// accounts get session_minted=false and sign in with their own password;
// expires_at is "" in that case.
export interface AcceptInviteResp {
  user_id: string
  email: string
  tenant_id: string
  session_minted: boolean
  livemode: boolean
  expires_at: string
}

export const membersApi = {
  list: () => apiRequest<MembersListResp>('GET', '/members/'),

  invite: (email: string) =>
    apiRequest<InvitationView>('POST', '/members/invite', { email }),

  revokeInvitation: (id: string) =>
    apiRequest<void>('DELETE', `/members/invitations/${id}`),

  removeMember: (userID: string) =>
    apiRequest<void>('DELETE', `/members/${userID}`),

  previewInvite: (token: string) =>
    apiRequest<InvitePreview>('GET', `/auth/invite/${token}`),

  acceptInvite: (token: string, password: string) =>
    apiRequest<AcceptInviteResp>('POST', '/auth/accept-invite', {
      token,
      password,
    }),
}
