# ADR-081: Minimal team invites (no RBAC)

Date: 2026-07-06
Status: Accepted (amended 2026-11-10)
Amends: ADR-011 (which shipped single-user auth and deferred invites)
Trigger: DP-readiness cut-reinstatement #2 ("Single dashboard seat" in the
DP-readiness register, maintained in the internal `velox-ops` repo) — a
self-hosted pilot puts the installing platform
engineer, the billing operator, and finance on one shared password within
30 days, and the append-only audit log becomes attribution-blind with one
user, undercutting a feature the README sells.

## Summary

Teammates join an account through a single-use email invite (`POST /v1/members/invite`) valid for 7 days, so people stop sharing one password and audit rows name the real person. There is no role-based access control: every member has full access, and the recorded role is not enforced. Only a signed-in dashboard user can invite or remove members, because an API key has no person to attribute the act to. Removing a member ends all their sessions, and nobody can remove themselves or the last member, even when two removals run at once. An invitation is saved with its email, and an accept saves the account and the membership with its claim on the invitation, so a failure leaves nothing half done (amendment 2026-11-10). Status: active, amending [ADR-011](011-email-password-auth-and-clean-api-keys.md); roles, MFA and SSO are not built.

## Decision

Ship the smallest thing that kills the shared password; keep RBAC cut.

1. **Invite by email, tokenized accept.** `POST /v1/members/invite` creates
   a `member_invitations` row (migration 0139; the 0035 ancestor belonged
   to the cut Members feature and was dropped in 0068) and sends an accept
   link through the transactional email outbox. Token discipline mirrors
   password reset: 32 random bytes, only sha256 stored, single-use via CAS
   on `accepted_at`, 7-day TTL (invites mint a NEW account for a colleague
   possibly on PTO; reset is 1h because it targets an existing one). One
   PENDING invite per (tenant, email), enforced by a partial-unique index.

2. **No RBAC — role recorded, not enforced.** Every member's session gets
   the full owner permission set (`auth.KeyTypeSession`, unchanged).
   Invited users are recorded as `role='member'` (user_tenants CHECK
   expanded owner→owner/admin/member in 0139) so the future role split has
   honest data, but nothing reads the role today and the UI says so.

3. **Membership endpoints are dashboard-session-only** (`auth.RequireSession`).
   Inviting and removing humans is a human act; API keys carry no user
   identity to attribute it to, and the audit trail needs a person.

4. **Accept is trust-tiered.** A NEW account sets its password during
   accept and gets a session minted (same trust as a completed password
   reset). An EXISTING account is attached but NOT logged in — possession
   of an inbox must not open an already-privileged account; they sign in
   with their own password.

5. **Removal revokes sessions.** Sessions pin (user, tenant), so deleting
   the membership row alone leaves live sessions working until expiry;
   `RemoveMember` calls `sessions.RevokeAllForUser`. Guards: no
   self-removal (lockout footgun), never the last member (unrecoverable
   workspace without psql).

6. **No enumeration oracles.** Invalid, expired, revoked, and consumed
   tokens all collapse into one generic 422. The accept endpoints ride the
   `/v1/auth` rate-limit block so token guessing is throttled like
   credential stuffing.

7. **Email failure is loud.** If the outbox enqueue fails, the invitation
   row is revoked before the error returns — a pending row with no email
   is a dead end that also blocks re-invites via the partial-unique index.
   Invites refuse to mint at all when `DASHBOARD_BASE_URL` is unset
   (accept links are never derived from request headers — same
   host-header-poisoning posture as password reset).

## Still cut (unchanged)

Role enforcement, per-role permission maps, invitation resend (revoke +
re-invite covers it), 2FA, SSO (ADR-014 direction stands). The permission
map swap-over point is documented at `internal/auth/permission.go`.

## Consequences

- Two humans on one tenant no longer share a credential; audit rows carry
  real per-person actors (`member.invited`, `member.joined`,
  `member.removed`, `member.invite_revoked`).
- When RBAC lands, users recorded as `member` will visibly LOSE access
  they implicitly had — the UI copy ("every member has full access — 
  per-role permissions are coming later") pre-frames that.
- v1's "each user belongs to exactly one tenant" login assumption
  (user/handler.go picks `tenants[0]`) now has a real multi-tenant case:
  an existing user accepting an invite to a second workspace. 0139 adds
  `user_tenants.created_at` and TenantsForUser orders by it, so login
  deterministically lands on the FIRST-joined workspace; a tenant
  switcher is future work and will ride the session's TenantID (already
  per-session, not per-user). On a single-tenant install — the pilot
  shape — the only existing-user accept is a removed member re-invited,
  which round-trips cleanly.

## Amendment 2026-11-10: each membership change is one transaction

Three membership writes were split across transactions, so a race or a
crash between the parts could leave a half-done state. Each is now one
transaction.

- **Removal (decision 5).** The last-member check counted members in one
  transaction and deleted in another. Two members removing each other at
  once both saw two members, and the account was left with none. Now
  `PostgresStore.RemoveMember` takes a per-tenant advisory lock
  (`user_tenants:<tenant>`, held to commit) and deletes with the count in
  the `DELETE` itself. The second removal waits, sees one member, and gets
  the same "cannot remove the last member" refusal. Session revocation is
  unchanged.
- **Accept (decision 1).** The accept used up the invitation in one
  transaction, then created the account and the membership in two more. A
  failure in between burned the token, and could leave an account in no
  workspace. Now the claim on `accepted_at`, the account and the
  membership commit together. A failed accept writes nothing, so the
  invitee can use the link again. The password is still checked, and now
  hashed, before the claim.
- **Invite email (decision 7).** The email was queued after the
  invitation committed, and a failed queue revoked the invitation. A crash
  in between left a pending invitation whose link was never sent. Now the
  `email_outbox` row is written on the invitation's transaction, which
  runs under `TxTenant` so the row gets the mode. A failed queue writes no
  invitation, and the revoke is gone.
