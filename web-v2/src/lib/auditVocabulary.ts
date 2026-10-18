import type { AuditEntry } from '@/lib/api'

// Audit-log display vocabulary — the frontend half of the ADR-090 contract.
//
// The backend's action / resource_type wire strings are FROZEN (ADR-090 §2):
// historical rows, the /audit-logs filter dropdowns and these maps all key on
// them. That makes this file a MIRROR of what the Go writers emit, and a
// mirror can drift: before this sweep, every ADR-081 membership and auth event
// (member.joined, login, mode_changed, …) fell through to the default arm and
// rendered as a raw dotted string like "member.joined sagar@example.com", and
// member.removed — which revokes the target's sessions — got no destructive
// styling at all.
//
// The list below was swept from the actual .Log / .LogInTx call sites. When you
// add a writer, add its action here. Kept separate from AuditLog.tsx so it is
// unit-testable without mounting React.

/**
 * describeAction renders one audit row's headline. The actor is rendered
 * separately by the page, so these strings describe the ACTION, not who did it.
 */
// Bulk data egress (action=export, ADR-090) names the resource that LEFT, so the
// row reads "Exported customers (CSV)" rather than a raw wire string. The row
// carries no resource_id — a bulk export has no single subject — so the
// resource_type is the whole story.
const EXPORTED_RESOURCE_LABELS: Record<string, string> = {
  customer: 'customers',
  invoice: 'invoices',
  subscription: 'subscriptions',
  usage_event: 'usage events',
  audit_log: 'the audit log',
}

export function describeAction(entry: AuditEntry): string {
  const label = entry.resource_label || ''
  // Item-level audit rows carry the meaningful discriminator in
  // metadata.action (item_plan_changed / item_quantity_changed) —
  // surface it cleanly instead of dumping the raw dotted action.
  const metaAction = (entry.metadata?.action as string) || ''
  switch (entry.action) {
    case 'export':
      return `Exported ${EXPORTED_RESOURCE_LABELS[entry.resource_type] ?? entry.resource_type} (CSV)`
    case 'create':
      if (entry.resource_type === 'payment_method') return `Added ${label || 'card'}`
      if (entry.resource_type === 'api_key') return `Created API key${label ? ` "${label}"` : ''}`
      if (entry.resource_type === 'webhook_endpoint') return `Created webhook endpoint${label ? ` ${label}` : ''}`
      if (entry.resource_type === 'test_clock') return `Created test clock${label ? ` "${label}"` : ''}`
      if (entry.resource_type === 'stripe_credentials') return `Connected Stripe (${(entry.metadata?.livemode as boolean) ? 'live' : 'test'})`
      return `Created ${label || entry.resource_type}`
    case 'update':
      // Sub-action discriminator for the update bucket: surface a
      // descriptive label per metadata.action when the bucket carries
      // a known transition. Falls through to generic "Updated X" for
      // anything not enumerated.
      if (entry.resource_type === 'invoice' && metaAction === 'marked_uncollectible') return `Marked ${label || 'invoice'} uncollectible`
      if (entry.resource_type === 'invoice' && metaAction === 'payment_recorded') return `Recorded offline payment on ${label || 'invoice'}`
      if (entry.resource_type === 'invoice' && metaAction === 'portal_pay_attempted') return `Customer paid ${label || 'invoice'} via portal`
      if (entry.resource_type === 'customer' && metaAction === 'profile_updated') return `Updated profile${label ? ` for ${label}` : ''}`
      if (entry.resource_type === 'customer' && metaAction === 'billing_profile_upserted') return `Updated billing profile${label ? ` for ${label}` : ''}`
      // The operator-driven and engine-fired "send a payment-setup link" rows
      // both land in the update/customer bucket (paymentmethods.Handler and the
      // finalize-with-no-PM adapter). Neither carries the recipient address —
      // customer PII must not enter the append-only log (TestAudit_NoCustomerPIIInMetadata);
      // the email outbox is the delivery record.
      if (entry.resource_type === 'customer' && metaAction === 'setup_link_sent') {
        return entry.metadata?.trigger === 'finalize_no_pm'
          ? `Emailed a payment-method setup link${label ? ` to ${label}` : ''} (invoice finalized with no card on file)`
          : `Sent a payment-method setup link${label ? ` to ${label}` : ''}`
      }
      if (entry.resource_type === 'payment_method' && metaAction === 'default_changed') return `Set ${label || 'card'} as default`
      if (entry.resource_type === 'subscription' && metaAction === 'cancel_cleared') return `Cleared scheduled cancellation${label ? ` on ${label}` : ''}`
      if (entry.resource_type === 'subscription' && metaAction === 'trial_ended') return `Trial ended${label ? ` on ${label}` : ''}`
      if (entry.resource_type === 'subscription' && metaAction === 'item_added') return `Added item${label ? ` to ${label}` : ''}`
      if (entry.resource_type === 'subscription' && metaAction === 'item_removed') return `Removed item${label ? ` from ${label}` : ''}`
      if (entry.resource_type === 'setting' && metaAction === 'settings_updated') {
        const n = entry.metadata?.changed ? Object.keys(entry.metadata.changed as Record<string, unknown>).length : 0
        return n > 0 ? `Updated settings (${n} field${n === 1 ? '' : 's'})` : 'Updated settings'
      }
      if (entry.resource_type === 'dunning_run' && metaAction === 'resolved') return `Resolved dunning run (${entry.metadata?.resolution})`
      if (entry.resource_type === 'dunning_policy' && metaAction === 'set_default') return `Set dunning policy as default${label ? ` (${label})` : ''}`
      if (entry.resource_type === 'test_clock' && metaAction === 'advanced') return `Advanced test clock${label ? ` "${label}"` : ''}`
      if (entry.resource_type === 'test_clock' && metaAction === 'retry_advance') return `Retried clock advance${label ? ` on "${label}"` : ''}`
      if (entry.resource_type === 'webhook_event' && metaAction === 'replayed') return `Replayed webhook event`
      if (entry.resource_type === 'stripe_credentials' && metaAction === 'webhook_secret_set') return `Set Stripe webhook secret (${(entry.metadata?.livemode as boolean) ? 'live' : 'test'})`
      return `Updated ${label || entry.resource_type}`
    case 'delete':
      if (entry.resource_type === 'payment_method') return `Removed ${label || 'card'}`
      if (entry.resource_type === 'webhook_endpoint') return `Deleted webhook endpoint`
      if (entry.resource_type === 'test_clock') return `Deleted test clock`
      if (entry.resource_type === 'dunning_policy') return `Deleted dunning policy`
      if (entry.resource_type === 'stripe_credentials') return `Disconnected Stripe (${(entry.metadata?.livemode as boolean) ? 'live' : 'test'})`
      return `Deleted ${label || entry.resource_type}`
    case 'activate': return `Activated ${label || 'subscription'}`
    case 'cancel': return `Canceled ${label || 'subscription'}`
    case 'pause': return `Paused ${label || 'subscription'}`
    case 'resume': return `Resumed ${label || 'subscription'}`
    case 'finalize': return `Finalized ${label || 'invoice'}`
    case 'void': return `Voided ${label || 'invoice'}`
    case 'collect': return `Collected payment on ${label || 'invoice'}`
    case 'send': return `Emailed ${label || 'invoice'}`
    case 'refund': return `Refunded ${label || 'invoice'}`
    case 'retry_tax': return `Retried tax on ${label || 'invoice'}`
    case 'issue': return `Issued ${label || 'credit note'}`
    case 'resolve': return `Resolved ${label || 'dunning run'}`
    case 'grant': return `Granted credits${label ? ` to ${label}` : ''}`
    case 'adjust': return `Adjusted credits${label ? ` for ${label}` : ''}`
    case 'credit.adjustment': return `Adjusted credits${label ? ` for ${label}` : ''}`
    case 'credit.deduction': return `Deducted credits${label ? ` from ${label}` : ''}`
    case 'credit_note.issued': return `Issued credit note ${label}`
    case 'subscription.item_updated':
      if (metaAction === 'item_plan_changed') return `Changed plan${label ? ` for ${label}` : ''}`
      if (metaAction === 'item_quantity_changed') return `Changed quantity${label ? ` for ${label}` : ''}`
      return `Updated item${label ? ` on ${label}` : ''}`
    case 'subscription.proration_failed': return `Proration failed${label ? ` on ${label}` : ''}`
    case 'subscription.pending_change_applied': return `Applied scheduled change${label ? ` to ${label}` : ''}`
    case 'subscription.threshold_crossed': return `Billing threshold crossed${label ? ` on ${label}` : ''}`
    case 'subscription.threshold_deferred': return `Billing threshold deferred${label ? ` on ${label}` : ''}`
    case 'subscription.net_negative_usage': return `Usage below zero billed as zero${label ? ` on ${label}` : ''}`
    case 'revoke': return `Revoked API key${label ? ` "${label}"` : ''}`
    case 'rotate':
      if (entry.resource_type === 'api_key') return `Rotated API key${label ? ` "${label}"` : ''}`
      if (entry.resource_type === 'webhook_endpoint') return `Rotated webhook secret`
      if (entry.resource_type === 'stripe_credentials') return `Rotated Stripe webhook secret`
      if (entry.resource_type === 'invoice') return `Rotated hosted-invoice link${label ? ` for ${label}` : ''}`
      return `Rotated ${label || entry.resource_type}`
    case 'run': return 'Billing cycle executed'
    case 'change_plan': return `Changed plan${label ? ` for ${label}` : ''}`

    // Team membership (ADR-081). resource_type is 'user'; resource_label is the
    // invitee/member email where one is known, '' on revoke/remove.
    case 'member.invited': return `Invited ${label || 'a team member'}`
    case 'member.joined': return `Joined the team${label ? ` (${label})` : ''}`
    case 'member.invite_revoked': return 'Revoked a team invitation'
    case 'member.removed': return 'Removed a team member'

    // Dashboard auth events (ADR-011). Actor is the operator (or 'system' for a
    // reset request, which any unauthenticated party can trigger).
    case 'login': return 'Signed in'
    case 'logout': return 'Signed out'
    case 'mode_changed': return `Switched to ${(entry.metadata?.livemode as boolean) ? 'live' : 'test'} mode`
    case 'password_reset_requested': return `Requested a password reset${label ? ` for ${label}` : ''}`
    case 'password_reset_completed': return 'Completed a password reset'

    default: return `${entry.action.replace(/_/g, ' ')} ${label || entry.resource_type}`
  }
}

// Destructive or irreversible actions — rendered with the danger accent.
// member.removed belongs here: it strips a person's access and revokes their
// live sessions, which is as consequential as revoking an API key.
export const HIGH_SEVERITY = new Set([
  'void', 'cancel', 'delete', 'revoke', 'credit.deduction', 'refund',
  'member.removed',
])

// Money-moving or security-relevant, but not destructive.
export const MEDIUM_SEVERITY = new Set([
  'finalize', 'grant', 'issue', 'credit_note.issued', 'change_plan',
  'subscription.item_updated', 'collect',
  'member.invite_revoked', 'password_reset_completed',
  // A bulk export copies an entire tenant's customer PII / invoices / usage out
  // of the system. It is the only READ Velox records at all (ADR-090 §7), and it
  // is the row an auditor scans for — it must not render as an unaccented,
  // routine-looking line.
  'export',
])

/**
 * resourceLink is the "View" target for a row. Returns null when the resource
 * has no detail page — rendering a link to a route that doesn't exist would
 * land the operator on a blank screen.
 */
export function resourceLink(entry: AuditEntry): string | null {
  // Guard the empty-resource_id case — some audit rows (e.g. tenant-scope
  // events, or events written before a child resource exists) carry an
  // empty resource_id, and rendering "View" → /customers/ would land the
  // user on a broken page.
  if (!entry.resource_id) return null
  // The subject outlived by the row. Tearing down a test clock hard-deletes its
  // entire simulated graph (ADR-086) while the audit rows survive on purpose —
  // so the row still names an invoice that no longer exists, and the link we used
  // to render for it went straight to a 404. The backend resolves this at read
  // time; we just have to stop offering the door.
  if (entry.subject_deleted) return null
  switch (entry.resource_type) {
    case 'invoice': return `/invoices/${entry.resource_id}`
    case 'customer': return `/customers/${entry.resource_id}`
    case 'subscription': return `/subscriptions/${entry.resource_id}`
    case 'plan': return `/plans/${entry.resource_id}`
    case 'meter': return `/meters/${entry.resource_id}`
    // Test clocks have had a detail route (/test-clocks/:id, main.tsx) since the
    // clock UI shipped, but their audit rows never offered the "View" link.
    case 'test_clock': return `/test-clocks/${entry.resource_id}`
    default: return null
  }
}

// Fallbacks for an empty tenant: without any audit rows the /filters endpoint
// returns [], leaving the dropdowns blank. These lists give a new tenant the
// common vocabulary (it's a hint, not a contract — the server list REPLACES
// them wholesale as soon as it is non-empty), so they mirror what the writers
// actually emit rather than an arbitrary subset.
export const DEFAULT_RESOURCE_TYPES = [
  'customer', 'subscription', 'invoice', 'plan', 'meter',
  'credit', 'credit_note', 'api_key', 'billing', 'billing_profile',
  'payment_method', 'dunning_policy', 'dunning_run', 'test_clock',
  'webhook_endpoint', 'webhook_event', 'stripe_credentials',
  'price_override', 'rating_rule', 'meter_pricing_rule',
  // Emitted by the ADR-090 in-tx writers. (The dropdown normally comes from
  // /filters — these only matter as the empty-log fallback.) Historical rows
  // from the retired catch-all used the hyphenated 'provider-cost'; the
  // snake_case form is the one every writer emits now.
  'provider_cost', 'recipe', 'tenant', 'user',
  // 'setting' — tenant settings saves (tenant/settings.go).
  // 'usage_event' — operator usage backfill (usage/service.go).
  'setting', 'usage_event',
  // Only ever appears on export rows.
  'audit_log',
]

export const DEFAULT_ACTIONS = [
  'create', 'update', 'delete', 'activate', 'cancel', 'pause', 'resume',
  'finalize', 'void', 'run', 'grant', 'revoke',
  // Read egress — the CSV exports. The only action that records a READ, and the
  // one an auditor asks for by name.
  'export',
  // The rest of the emitted vocabulary. The old list stopped at 'revoke', so a
  // fresh tenant's dropdown silently omitted every invoice money action, the
  // credit ledger, membership and auth.
  'refund', 'collect', 'send', 'retry_tax', 'rotate',
  'credit.adjustment', 'credit.deduction', 'credit_note.issued',
  'subscription.item_updated', 'subscription.pending_change_applied',
  'subscription.proration_failed', 'subscription.threshold_crossed',
  'subscription.threshold_deferred', 'subscription.net_negative_usage',
  'member.invited', 'member.joined', 'member.invite_revoked', 'member.removed',
  'login', 'logout', 'mode_changed',
  'password_reset_requested', 'password_reset_completed',
]
