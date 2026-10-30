import { useState } from 'react'
import { useMutation } from '@tanstack/react-query'
import { useForm } from 'react-hook-form'
import { ExternalLink, Loader2 } from 'lucide-react'
import { api, formatCents, type CreditNote } from '@/lib/api'
import { attachStripeRefundToCreditNote, dismissInvoiceStripeRefund } from '@/lib/gen/queries.gen'
import type { InvoiceStripeRefund } from '@/lib/gen/schemas/invoiceStripeRefund'
import { formatInvoiceDate } from '@/lib/dates'
import { attachStripeRefundOutcome, recordStripeRefundOutcome } from '@/lib/actionOutcomes'
import { showOutcome } from '@/lib/outcomeToast'
import { applyApiError, showApiError } from '@/lib/formErrors'
import { stripePaymentUrl } from '@/lib/invoiceNotice'
import {
  dismissNoteError,
  flaggedStripeRefunds,
  recordStripeRefundRequest,
  stripeRefundChoices,
} from '@/lib/stripeRefunds'
import { useAuth } from '@/contexts/AuthContext'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'

// The invoice fields these actions read.
interface RefundInvoice {
  id: string
  invoice_number: string
  status: string
  payment_status: string
  billing_timezone?: string | null
}

// StripeRefundActions is the buttons for the refunds made in Stripe that
// Hardcount flagged on this invoice (ADR-134), drawn in the invoice notice's
// button row: Record as credit note, Attach to <note> (when a credit note's
// own refund failed and this refund may replace it), and Dismiss. Each opens
// a confirmation first. One refund gets plain buttons; several get one row
// each, naming the refund. lib/stripeRefunds decides which are offered.
export function StripeRefundActions({ invoice, refunds, creditNotes, primary, onChanged }: {
  invoice: RefundInvoice
  refunds: InvoiceStripeRefund[] | undefined
  creditNotes: CreditNote[]
  // Whether the first button is the notice's main one. False when the
  // notice already has the server's own button first.
  primary: boolean
  onChanged: () => void
}) {
  const flagged = flaggedStripeRefunds(refunds)
  // One Idempotency-Key per Record dialog opening, as Issue credit note does:
  // a retry after a lost answer gets the first note back.
  const [recordTarget, setRecordTarget] = useState<{ refund: InvoiceStripeRefund; key: string } | null>(null)
  const [attachTarget, setAttachTarget] = useState<{ refund: InvoiceStripeRefund; note: CreditNote } | null>(null)
  const [dismissTarget, setDismissTarget] = useState<InvoiceStripeRefund | null>(null)

  const recordMutation = useMutation({
    mutationFn: (t: { refund: InvoiceStripeRefund; key: string }) =>
      api.createCreditNote(recordStripeRefundRequest(invoice.id, t.refund), t.key),
    onSuccess: (cn) => { setRecordTarget(null); onChanged(); showOutcome(recordStripeRefundOutcome(cn)) },
    onError: (err) => { setRecordTarget(null); onChanged(); showApiError(err, "Couldn't record the refund") },
  })
  const attachMutation = useMutation({
    mutationFn: (t: { refund: InvoiceStripeRefund; note: CreditNote }) =>
      attachStripeRefundToCreditNote(t.note.id, { stripe_refund_id: t.refund.stripe_refund_id }),
    onSuccess: (cn) => { setAttachTarget(null); onChanged(); showOutcome(attachStripeRefundOutcome(cn)) },
    onError: (err) => { setAttachTarget(null); onChanged(); showApiError(err, "Couldn't attach the refund") },
  })

  if (flagged.length === 0) return null
  const several = flagged.length > 1
  const amount = (r: InvoiceStripeRefund) => formatCents(r.amount_cents, r.currency)
  const day = (r: InvoiceStripeRefund) => formatInvoiceDate(r.refund_created_at, invoice.billing_timezone)

  return (
    <>
      {flagged.map((r, i) => {
        const choices = stripeRefundChoices(r, invoice, creditNotes)
        const buttons = [
          ...choices.attachTo.map(note => ({
            key: `attach-${note.id}`,
            label: `Attach to ${note.credit_note_number}`,
            onClick: () => setAttachTarget({ refund: r, note: note as CreditNote }),
          })),
          ...(choices.record ? [{
            key: 'record',
            label: 'Record as credit note',
            onClick: () => setRecordTarget({ refund: r, key: crypto.randomUUID() }),
          }] : []),
          ...(choices.dismiss ? [{ key: 'dismiss', label: 'Dismiss', onClick: () => setDismissTarget(r) }] : []),
        ]
        return (
          <div
            key={r.stripe_refund_id}
            role="group"
            aria-label={`Refund of ${amount(r)} made in Stripe on ${day(r)}`}
            className={several ? 'flex w-full flex-wrap items-center gap-2' : 'flex flex-wrap items-center gap-2'}
          >
            {several && (
              <span className="text-xs text-muted-foreground">
                {amount(r)} on {day(r)}
              </span>
            )}
            {buttons.map((b, j) => (
              <Button key={b.key} size="sm" variant={primary && i === 0 && j === 0 ? 'default' : 'outline'} onClick={b.onClick}>
                {b.label}
              </Button>
            ))}
          </div>
        )
      })}

      <AlertDialog open={!!recordTarget} onOpenChange={(open) => { if (!open && !recordMutation.isPending) setRecordTarget(null) }}>
        <AlertDialogContent>
          {recordTarget && (
            <>
              <AlertDialogHeader>
                <AlertDialogTitle>Record {amount(recordTarget.refund)} as a credit note?</AlertDialogTitle>
                <AlertDialogDescription>
                  A refund of {amount(recordTarget.refund)} was made in Stripe on {day(recordTarget.refund)}. Hardcount
                  records it on {invoice.invoice_number} as a credit note whose card refund is that refund.
                </AlertDialogDescription>
              </AlertDialogHeader>
              <div className="space-y-2 text-sm text-muted-foreground">
                <p>The customer is not refunded again.</p>
                <p>The credit note is issued at once and can&apos;t be undone.</p>
              </div>
              <AlertDialogFooter>
                <AlertDialogCancel disabled={recordMutation.isPending}>Cancel</AlertDialogCancel>
                <AlertDialogAction onClick={() => recordMutation.mutate(recordTarget)} disabled={recordMutation.isPending}>
                  {recordMutation.isPending
                    ? <><Loader2 size={14} className="animate-spin mr-1.5" />Recording…</>
                    : `Record ${amount(recordTarget.refund)}`}
                </AlertDialogAction>
              </AlertDialogFooter>
            </>
          )}
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={!!attachTarget} onOpenChange={(open) => { if (!open && !attachMutation.isPending) setAttachTarget(null) }}>
        <AlertDialogContent>
          {attachTarget && (
            <>
              <AlertDialogHeader>
                <AlertDialogTitle>Attach this refund to {attachTarget.note.credit_note_number}?</AlertDialogTitle>
                <AlertDialogDescription>
                  The card refund of {formatCents(attachTarget.note.refund_amount_cents, attachTarget.refund.currency)} on{' '}
                  {attachTarget.note.credit_note_number} failed. The {amount(attachTarget.refund)} refund made in Stripe
                  on {day(attachTarget.refund)} becomes its card refund.
                </AlertDialogDescription>
              </AlertDialogHeader>
              <div className="space-y-2 text-sm text-muted-foreground">
                <p>No new credit note is made, and the customer is not refunded again.</p>
                <p>Do this only if the refund in Stripe was meant to replace the one that failed.</p>
              </div>
              <AlertDialogFooter>
                <AlertDialogCancel disabled={attachMutation.isPending}>Cancel</AlertDialogCancel>
                <AlertDialogAction onClick={() => attachMutation.mutate(attachTarget)} disabled={attachMutation.isPending}>
                  {attachMutation.isPending
                    ? <><Loader2 size={14} className="animate-spin mr-1.5" />Attaching…</>
                    : `Attach to ${attachTarget.note.credit_note_number}`}
                </AlertDialogAction>
              </AlertDialogFooter>
            </>
          )}
        </AlertDialogContent>
      </AlertDialog>

      {dismissTarget && (
        <DismissStripeRefundDialog
          invoiceId={invoice.id}
          refund={dismissTarget}
          amountLabel={amount(dismissTarget)}
          dayLabel={day(dismissTarget)}
          onClose={() => setDismissTarget(null)}
          onDismissed={() => {
            setDismissTarget(null)
            onChanged()
            showOutcome({ tone: 'success', title: 'Refund dismissed', description: 'Your note is kept with the refund and in the audit log.' })
          }}
        />
      )}
    </>
  )
}

// DismissStripeRefundDialog asks how the operator handled the refund; the
// note is required (the server keeps it on the refund and in the audit log).
// Mounted only while open, so each opening starts empty.
function DismissStripeRefundDialog({ invoiceId, refund, amountLabel, dayLabel, onClose, onDismissed }: {
  invoiceId: string
  refund: InvoiceStripeRefund
  amountLabel: string
  dayLabel: string
  onClose: () => void
  onDismissed: () => void
}) {
  const form = useForm<{ note: string }>({ defaultValues: { note: '' } })
  const { register, handleSubmit, setError, formState: { errors, isSubmitting } } = form

  const onSubmit = handleSubmit(async ({ note }) => {
    const problem = dismissNoteError(note)
    if (problem) {
      setError('note', { type: 'client', message: problem }, { shouldFocus: true })
      return
    }
    try {
      await dismissInvoiceStripeRefund(invoiceId, refund.stripe_refund_id, { note: note.trim() })
      onDismissed()
    } catch (err) {
      applyApiError(form, err, ['note'], { toastTitle: "Couldn't dismiss the refund" })
    }
  })

  return (
    <Dialog open onOpenChange={(open) => { if (!open && !isSubmitting) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Dismiss this refund?</DialogTitle>
          <DialogDescription>
            For a refund you handled outside Hardcount: the {amountLabel} refund made in Stripe on {dayLabel}.
            No credit note is made, so this invoice and its PDF still won&apos;t show the refund.
          </DialogDescription>
        </DialogHeader>
        <form onSubmit={onSubmit} noValidate className="space-y-4">
          <div className="space-y-2">
            <Label htmlFor="dismiss-refund-note">How you handled it</Label>
            <Textarea
              id="dismiss-refund-note"
              placeholder="Recorded in our books"
              aria-invalid={errors.note ? true : undefined}
              {...register('note')}
            />
            {errors.note
              ? <p className="text-xs text-destructive">{errors.note.message}</p>
              : <p className="text-xs text-muted-foreground">Kept with the refund and in the audit log.</p>}
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose} disabled={isSubmitting}>Cancel</Button>
            <Button type="submit" disabled={isSubmitting}>
              {isSubmitting ? <><Loader2 size={14} className="animate-spin mr-2" />Dismissing…</> : 'Dismiss refund'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

// RecordedFromStripeTag marks a credit note whose card refund is a refund
// made in Stripe outside Hardcount (recorded_from_stripe). Hardcount sent no
// refund for it; the refund's id says which one it is.
// The text truncates when the tag is given less room than it needs.
export function RecordedFromStripeTag({ note, className }: {
  note: Pick<CreditNote, 'recorded_from_stripe' | 'stripe_refund_id'>
  className?: string
}) {
  if (!note.recorded_from_stripe) return null
  return (
    <Badge
      variant="info"
      className={className}
      title={note.stripe_refund_id ? `Recorded from Stripe: refund ${note.stripe_refund_id}` : 'Recorded from Stripe'}
    >
      <span className="truncate">Recorded from Stripe</span>
    </Badge>
  )
}

// StripeRefundLine names the refund a recorded credit note holds, with a link
// to the refunded payment in the Stripe dashboard (the payment page, which
// lists its refunds; the app links Stripe payments, not refunds).
export function StripeRefundLine({ refundId, paymentIntentId }: { refundId?: string; paymentIntentId?: string | null }) {
  const { user } = useAuth()
  if (!refundId) return null
  return (
    <span className="flex flex-wrap items-center gap-x-2 gap-y-0.5">
      <span className="font-mono break-all">Stripe refund {refundId}</span>
      {paymentIntentId && (
        <a
          href={stripePaymentUrl(paymentIntentId, user?.livemode ?? false)}
          target="_blank"
          rel="noopener noreferrer"
          className="inline-flex items-center gap-0.5 text-primary hover:underline"
        >
          Open in Stripe <ExternalLink size={10} aria-hidden />
        </a>
      )}
    </span>
  )
}
