import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { AlertTriangle, AlertCircle, Info, ExternalLink, Calendar } from 'lucide-react'
import { api, formatCents, type Invoice } from '@/lib/api'
import { SendSetupLinkDialog } from '@/components/SendSetupLinkDialog'
import type {
  AttentionAction,
} from '@/lib/api'
import { formatDate, formatDateTime } from '@/lib/api'
import { timeAgo, isOlderThan, type EffectiveNow } from '@/lib/effectiveNow'
import { Callout } from '@/components/Callout'
import { toneForSeverity } from '@/lib/calloutTone'
import { attentionReasonLabel, supportCodeLine } from '@/lib/attentionLabels'
import { Button } from '@/components/ui/button'

// InvoiceAttention is the unified "this invoice needs operator
// attention" notice, drawn with the shared Callout. Reads server-derived
// invoice.attention and renders nothing when absent (healthy or
// terminal-state): reason label as the title, the server's message,
// prescribed actions, and a collapsed Details holding the explanation and
// the support code.
//
// Stays purely presentational: the typed reason / action / severity
// codes come from the server via ADR-009. Adding a new code on the
// server doesn't require touching this component (the default action
// renderer covers any AttentionAction).
export function InvoiceAttention({
  invoice,
  onRetryTax,
  onChargeNow,
  onSendReminder,
  onVoidInvoice,
  retrying,
  charging,
  sending,
  now,
}: {
  invoice: Invoice
  onRetryTax?: () => void
  onChargeNow?: () => void
  onSendReminder?: () => void
  // Opens the invoice's existing void confirmation. Surfaced only on
  // prepaid-commit exposure, where voiding is the sole way to stop credit
  // going out against an invoice nobody paid (ADR-078).
  onVoidInvoice?: () => void
  retrying?: boolean
  charging?: boolean
  sending?: boolean
  // ADR-030 / simulated-time discipline: when the owning subscription
  // is pinned to a test clock, relative-time renders ("since X ago")
  // must be computed against the clock's frozen_time, NOT wall-clock.
  // Wall-clock would surface absurd deltas like "833d ago" on a
  // newly-finalized invoice whose sub is frozen at 2024-02-01 while
  // wall-clock sits in 2026. Build the anchor with effectiveNow(...) —
  // the type makes passing it non-optional so this can't regress.
  now: EffectiveNow
}) {
  const att = invoice.attention
  if (!att) return null

  const tone = toneForSeverity(att.severity)
  const Icon = severityIcons[att.severity] ?? Info
  const supportCode = supportCodeLine(att.code)

  return (
    <Callout
      tone={tone}
      icon={Icon}
      className="mb-6"
      title={
        <span className="flex flex-wrap items-center gap-2">
          {attentionReasonLabel(att.reason, att.code)}
          {/* `since` reads as duration but is sourced from
              inv.UpdatedAt — for fresh invoices that's the
              finalize moment. Hide for the first 24h to avoid the
              misleading "since 5m ago" copy on a just-finalized
              invoice; surface only when the state has actually
              persisted long enough to matter. */}
          {att.since && isOlderThan(att.since, 24 * 60 * 60 * 1000, now) && (
            <span className="text-xs font-normal text-muted-foreground">
              · since {timeAgo(att.since, now)}
            </span>
          )}
        </span>
      }
      actions={(att.actions?.length ?? 0) > 0 ? (
        <>
          {att.actions!.map((action, idx) => (
            <ActionButton
              key={action.code}
              action={action.code}
              label={action.label}
              primary={idx === 0}
              invoice={invoice}
              onRetryTax={onRetryTax}
              onChargeNow={onChargeNow}
              onSendReminder={onSendReminder}
              onVoidInvoice={onVoidInvoice}
              retrying={retrying}
              charging={charging}
              sending={sending}
            />
          ))}
          {/* att.doc_url is not rendered: the pages it points at are not
              published yet, so a "Learn more" button would open a dead
              link. Restore the button when they exist. */}
        </>
      ) : undefined}
      // Details (ADR-025): Velox's own framing (att.detail) as plain text,
      // then the support code. The code used to sit on screen in grey mono
      // under the buttons; it only helps support, so it lives here, where a
      // screenshot of the opened notice still carries it.
      details={att.detail || supportCode ? (
        <>
          {att.detail && <p className="whitespace-pre-wrap break-words">{att.detail}</p>}
          {supportCode && <p>{supportCode}</p>}
        </>
      ) : undefined}
      // "Provider response" is the literal upstream payload from a third
      // party (Stripe etc.), kept in its own disclosure so an operator never
      // wonders "is this Velox talking or Stripe?". Populated only when we
      // actually called the provider.
      footer={att.provider_response ? (
        <details className="pt-1 text-xs text-muted-foreground">
          <summary className="cursor-pointer hover:text-foreground">
            Provider response
          </summary>
          <pre className="mt-2 p-2 bg-muted/50 rounded text-[10px] font-mono overflow-x-auto whitespace-pre-wrap break-all max-w-xl">
            {att.provider_response}
          </pre>
        </details>
      ) : undefined}
    >
      {/* The auto-collect ("attaching a card auto-charges…") reassurance
          is SERVER-OWNED — it lives inside att.message, where it is
          clock-aware (wall tick vs next test-clock advance). A second
          hardcoded copy here drifted from that truth and repeated the
          promise twice in one banner (2026-07-22 audit, P1-1/P2-1). */}
      <p className="text-foreground">{att.message}</p>
      {att.next_attempt_at && (
        <p className="text-xs text-muted-foreground flex items-center gap-1.5">
          <Calendar size={11} className="shrink-0" />
          Engine will retry: <span className="text-foreground">{formatDateTime(att.next_attempt_at)}</span>
        </p>
      )}
      {att.due_by && (
        // Date-only on purpose. due_at is computed as
        // finalize_at + net_payment_term_days; that arithmetic
        // carries finalize-time hour precision (e.g. "8:16 PM
        // GMT+5:30") which is meaningless to operators or
        // customers — the deadline is "this calendar date".
        // Stripe's banner uses date-only too.
        <p className="text-xs text-muted-foreground flex items-center gap-1.5">
          <Calendar size={11} className="shrink-0" />
          Due by: <span className="text-foreground">{formatDate(att.due_by)}</span>
        </p>
      )}
    </Callout>
  )
}

// ActionButton dispatches typed action codes to the right UI element.
// edit_billing_profile / review_registration / wait_provider /
// rotate_api_key are link-out actions; retry_tax is server-bound and
// uses the supplied callback. Unknown codes fall through to a labeled
// no-op button so a server-added action doesn't render as broken.
function ActionButton({
  action,
  label,
  primary,
  invoice,
  onRetryTax,
  onChargeNow,
  onSendReminder,
  onVoidInvoice,
  retrying,
  charging,
  sending,
}: {
  action: AttentionAction
  label?: string
  primary: boolean
  invoice: Invoice
  onRetryTax?: () => void
  onChargeNow?: () => void
  onSendReminder?: () => void
  onVoidInvoice?: () => void
  retrying?: boolean
  charging?: boolean
  sending?: boolean
}) {
  const variant = primary ? 'default' : 'outline'
  const display = label ?? defaultLabel(action)

  switch (action) {
    case 'void_invoice':
      // Destructive styling regardless of `primary`: void annuls an invoice,
      // and it is never the primary CTA here (it is appended after whatever
      // the cause banner already recommended). Rendering it in the same
      // neutral outline as "Retry tax" would understate what it does.
      return (
        <Button
          variant="destructive"
          size="sm"
          onClick={onVoidInvoice}
          disabled={!onVoidInvoice}
        >
          {display}
        </Button>
      )
    case 'edit_billing_profile':
    case 'add_payment_method':
      return (
        <Button render={<Link to={`/customers/${invoice.customer_id}`} />} variant={variant} size="sm">
          {display}
        </Button>
      )
    case 'retry_tax':
      return (
        <Button
          variant={variant}
          size="sm"
          onClick={onRetryTax}
          disabled={!onRetryTax || retrying}
        >
          {retrying ? 'Retrying…' : display}
        </Button>
      )
    case 'wait_provider':
      return (
        <Button render={<a href="https://status.stripe.com/" target="_blank" rel="noopener noreferrer" />} variant={variant} size="sm">
          {display}
          <ExternalLink size={12} className="ml-1" />
        </Button>
      )
    case 'rotate_api_key':
      return (
        <Button render={<Link to="/settings" />} variant={variant} size="sm">
          {display}
        </Button>
      )
    case 'review_registration':
      return (
        <Button render={<Link to="/settings" />} variant={variant} size="sm">
          {display}
        </Button>
      )
    case 'connect_tax_provider':
      // Deep-link to Settings → Payments tab. The Payments tab is
      // mode-scoped (see the Settings audit) so landing the operator
      // there in the active mode lets them connect the right
      // credentials without a second mode toggle.
      return (
        <Button render={<Link to="/settings?tab=payments" />} variant={variant} size="sm">
          {display}
        </Button>
      )
    case 'charge_now':
    case 'retry_payment':
      // Same semantic — call the engine to attempt the charge now.
      // retry_payment falls in here because the lower-banner pre-cleanup
      // used the same Collect endpoint; both verbs map to the same
      // `collectMutation` callback the parent owns.
      return (
        <Button
          variant={variant}
          size="sm"
          onClick={onChargeNow}
          disabled={!onChargeNow || charging}
        >
          {charging ? 'Charging…' : display}
        </Button>
      )
    case 'send_reminder':
      return (
        <Button
          variant={variant}
          size="sm"
          onClick={onSendReminder}
          disabled={!onSendReminder || sending}
        >
          {sending ? 'Sending…' : display}
        </Button>
      )
    case 'update_payment_method':
      return (
        <UpdatePaymentMethodButton
          variant={variant}
          display={display}
          invoice={invoice}
        />
      )
    case 'reconcile_payment':
    default:
      return (
        <Button variant={variant} size="sm" disabled>
          {display}
        </Button>
      )
  }
}

// UpdatePaymentMethodButton opens the shared SendSetupLinkDialog
// pre-loaded with invoice context — the customer hits an invoice
// they couldn't pay, operator nudges them with an email that
// references the specific invoice + amount due. Migrated from the
// legacy "open Stripe URL in operator's tab" flow (Path B in the
// old triple-flow audit) to the unified Path A backend so all PM
// link sends go through one service + one template + one audit row
// shape.
function UpdatePaymentMethodButton({ variant, display, invoice }: { variant: 'default' | 'outline'; display: string; invoice: Invoice }) {
  const [open, setOpen] = useState(false)
  // Lazy customer fetch: only triggers when the dialog opens, so we
  // don't pay the round-trip for every InvoiceAttention render.
  const { data: customer } = useQuery({
    queryKey: ['customer-for-setup-link', invoice.customer_id],
    queryFn: () => api.getCustomer(invoice.customer_id),
    enabled: open,
  })
  const amountDueLabel = formatCents(invoice.amount_due_cents, invoice.currency)
  return (
    <>
      <Button variant={variant} size="sm" onClick={() => setOpen(true)}>
        {display}
      </Button>
      <SendSetupLinkDialog
        open={open}
        onOpenChange={setOpen}
        customerId={invoice.customer_id}
        customerEmail={customer?.email}
        invoiceContext={{ invoiceNumber: invoice.invoice_number, amountDueLabel }}
      />
    </>
  )
}

// Module-scope lookup (not a function called during render): these are
// references to existing icon components, and a plain index makes that
// visible to both readers and the react-hooks/static-components rule —
// a render-time call assigned to a Capitalized JSX variable reads as
// creating a new component type per render.
const severityIcons: Record<string, typeof Info> = {
  critical: AlertCircle,
  warning: AlertTriangle,
  info: Info,
}

function defaultLabel(action: AttentionAction): string {
  const map: Record<AttentionAction, string> = {
    edit_billing_profile: 'Edit billing profile',
    retry_tax: 'Retry tax',
    retry_payment: 'Retry payment',
    wait_provider: 'Check provider status',
    rotate_api_key: 'Rotate API key',
    reconcile_payment: 'Reconcile',
    review_registration: 'Review tax registration',
    charge_now: 'Charge now',
    // send_reminder = server-side email to the customer with a
    // payment link. Verb is "Email" not "Share" — operators
    // mistakenly expected a clipboard action with the older "Share
    // invoice link" copy. The per-reason routing is CLIENT-side:
    // InvoiceDetail's onSendReminder branches on attention.reason
    // (no_payment_method → resend-setup-link endpoint, else → the
    // invoice email dialog); each backend endpoint sends one fixed
    // template.
    send_reminder: 'Email payment link',
    add_payment_method: 'Add payment method',
    update_payment_method: 'Update payment method',
    connect_tax_provider: 'Connect Stripe',
    void_invoice: 'Void invoice',
  }
  return map[action] ?? action
}

