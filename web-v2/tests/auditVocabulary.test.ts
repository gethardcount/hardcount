// Guards the frontend half of the ADR-090 audit vocabulary contract.
//
// The backend's action wire-strings are frozen; AuditLog.tsx mirrors them. The
// mirror had drifted: every ADR-081 membership event and every dashboard auth
// event fell through describeAction's default arm and rendered as a raw dotted
// string ("member.joined sagar@example.com"), and member.removed — which strips
// a person's access and revokes their sessions — carried no destructive styling.
// test_clock rows offered no "View" link despite /test-clocks/:id existing.
//
// EMITTED_ACTIONS below is swept from the Go .Log / .LogInTx call sites. If a
// writer adds an action and nobody updates auditVocabulary.ts, the first test
// fails.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import type { AuditEntry } from '../src/lib/api.ts'
import {
  describeAction,
  resourceLink,
  HIGH_SEVERITY,
  MEDIUM_SEVERITY,
  DEFAULT_ACTIONS,
} from '../src/lib/auditVocabulary.ts'

function entry(over: Partial<AuditEntry> = {}): AuditEntry {
  return {
    id: 'vlx_aud_1',
    actor_type: 'user',
    actor_id: 'vlx_usr_1',
    action: 'update',
    resource_type: 'customer',
    resource_id: 'vlx_cus_1',
    created_at: '2026-07-14T10:00:00Z',
    ...over,
  }
}

// Every action string a Go writer can emit (internal/**: .Log(...) 3rd arg and
// audit.Entry{Action: ...}). Keep in sync with the vocabulary round-trip list in
// internal/audit/logintx_integration_test.go.
const EMITTED_ACTIONS = [
  // domain.AuditAction* constants
  'create', 'update', 'delete', 'activate', 'cancel', 'pause', 'resume',
  'finalize', 'void', 'revoke', 'grant', 'refund', 'collect', 'send',
  'retry_tax', 'rotate', 'run',
  // credit ledger + credit notes
  'credit.adjustment', 'credit.deduction', 'credit_note.issued',
  // subscription lifecycle
  'subscription.item_updated', 'subscription.pending_change_applied',
  'subscription.proration_failed', 'subscription.threshold_crossed',
  'subscription.threshold_deferred', 'subscription.net_negative_usage',
  // team membership (ADR-081)
  'member.invited', 'member.joined', 'member.invite_revoked', 'member.removed',
  // dashboard auth (ADR-011)
  'login', 'logout', 'mode_changed',
  'password_reset_requested', 'password_reset_completed',
]

test('describeAction has a real case for every action the backend emits', () => {
  for (const action of EMITTED_ACTIONS) {
    const got = describeAction(entry({ action, resource_label: '' }))
    // The default arm echoes the wire string back. A dotted action surviving
    // into the UI is the exact regression this guards.
    assert.ok(
      !got.includes(action),
      `action "${action}" fell through to the default arm and rendered as "${got}"`,
    )
    assert.ok(got.length > 0, `action "${action}" rendered empty`)
  }
})

test('DEFAULT_ACTIONS (empty-tenant fallback) does not lie by omission', () => {
  for (const action of EMITTED_ACTIONS) {
    assert.ok(
      DEFAULT_ACTIONS.includes(action),
      `emitted action "${action}" missing from the DEFAULT_ACTIONS fallback`,
    )
  }
})

test('membership + auth events render as operator-readable copy, not dotted strings', () => {
  assert.equal(describeAction(entry({ action: 'member.joined', resource_type: 'user', resource_label: 'sam@acme.com' })),
    'Joined the team (sam@acme.com)')
  assert.equal(describeAction(entry({ action: 'member.invited', resource_type: 'user', resource_label: 'sam@acme.com' })),
    'Invited sam@acme.com')
  assert.equal(describeAction(entry({ action: 'member.removed', resource_type: 'user', resource_label: '' })),
    'Removed a team member')
  assert.equal(describeAction(entry({ action: 'member.invite_revoked', resource_type: 'user', resource_label: '' })),
    'Revoked a team invitation')
  assert.equal(describeAction(entry({ action: 'login', resource_type: 'user' })), 'Signed in')
  assert.equal(describeAction(entry({ action: 'logout', resource_type: 'user' })), 'Signed out')
  // mode_changed's only discriminator is metadata.livemode.
  assert.equal(describeAction(entry({ action: 'mode_changed', resource_type: 'user', metadata: { livemode: true } })),
    'Switched to live mode')
  assert.equal(describeAction(entry({ action: 'mode_changed', resource_type: 'user', metadata: { livemode: false } })),
    'Switched to test mode')
})

test('member.removed carries destructive styling; a plain invite does not', () => {
  // It revokes the target's sessions — same blast radius as revoking an API key.
  assert.ok(HIGH_SEVERITY.has('member.removed'), 'member.removed must be high severity')
  assert.ok(MEDIUM_SEVERITY.has('member.invite_revoked'))
  assert.ok(!HIGH_SEVERITY.has('member.invited') && !MEDIUM_SEVERITY.has('member.invited'),
    'inviting someone is not a destructive act')
})

test('test_clock rows deep-link to the clock detail page', () => {
  assert.equal(resourceLink(entry({ resource_type: 'test_clock', resource_id: 'vlx_tc_9' })), '/test-clocks/vlx_tc_9')
  // Every other mapped type still resolves.
  assert.equal(resourceLink(entry({ resource_type: 'invoice', resource_id: 'vlx_inv_1' })), '/invoices/vlx_inv_1')
  assert.equal(resourceLink(entry({ resource_type: 'customer', resource_id: 'vlx_cus_1' })), '/customers/vlx_cus_1')
  // No detail route → no "View" link (rendering one lands on a blank page).
  assert.equal(resourceLink(entry({ resource_type: 'user', resource_id: 'vlx_usr_1' })), null)
  // Empty resource_id is guarded even for a mapped type.
  assert.equal(resourceLink(entry({ resource_type: 'test_clock', resource_id: '' })), null)
})

test('a payment-setup-link row names the action, not a generic "Updated <customer>"', () => {
  // Operator-driven send (paymentmethods.Handler). No recipient address is on
  // the row by design — customer PII must not enter the append-only log.
  assert.equal(
    describeAction(entry({
      resource_type: 'customer', action: 'update', resource_label: 'Acme Corp',
      metadata: { action: 'setup_link_sent', session_id: 'cs_1' },
    })),
    'Sent a payment-method setup link to Acme Corp',
  )
  // Engine-fired at finalize when no card is on file — a distinct cause.
  assert.equal(
    describeAction(entry({
      resource_type: 'customer', action: 'update', resource_label: 'Acme Corp',
      metadata: { action: 'setup_link_sent', trigger: 'finalize_no_pm', invoice_id: 'vlx_inv_1' },
    })),
    'Emailed a payment-method setup link to Acme Corp (invoice finalized with no card on file)',
  )
})

test('a row whose subject was torn down offers no link', () => {
  // Deleting a test clock hard-deletes its whole simulated graph (ADR-086) while
  // the audit rows survive on purpose — so the row still names an invoice that no
  // longer exists. We used to render "View" on it, straight into a 404. The backend
  // resolves subject_deleted at read time; the only job here is to stop offering
  // the door.
  assert.equal(
    resourceLink(entry({ resource_type: 'invoice', resource_id: 'vlx_inv_sim', subject_deleted: true })),
    null,
    'a torn-down subject must not render a View link — it 404s')

  // The same row while its clock still lives: the subject is there, the link works.
  assert.equal(
    resourceLink(entry({ resource_type: 'invoice', resource_id: 'vlx_inv_sim', subject_deleted: false })),
    '/invoices/vlx_inv_sim')

  // Absent (every wall-clock row): unchanged, still links.
  assert.equal(
    resourceLink(entry({ resource_type: 'invoice', resource_id: 'vlx_inv_real' })),
    '/invoices/vlx_inv_real')
})
