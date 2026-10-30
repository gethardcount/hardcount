import { useState } from 'react'
import { api, formatCents, getCurrencySymbol, type CreditNote } from '@/lib/api'
import { creditNoteCeilings, hasCardPayment } from '@/lib/creditNoteCeilings'
import { amountInputError, dollarsToWholeCents } from '@/lib/priceInput'
import { useSingleFlight } from '@/hooks/useSingleFlight'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle,
} from '@/components/ui/dialog'
import { Loader2 } from 'lucide-react'

// The invoice fields the dialog reads. Both the invoice page and the Credit
// Notes page pass an invoice they loaded; this is all the dialog needs of it.
export interface CreditableInvoice {
  id: string
  invoice_number: string
  status: string
  currency: string
  total_amount_cents: number
  amount_paid_cents: number
  stripe_payment_intent_id?: string | null
}

// The one dialog that issues a credit note against an invoice. The invoice
// page opens it from its actions menu; the Credit Notes page opens it once an
// invoice is picked. Before, the Credit Notes page had a second dialog of its
// own that drifted: a Max that ignored earlier notes, a refund option the
// server always refused, and amounts in the workspace currency (D-13).
export function IssueCreditDialog({ invoice, existingCreditNotes, onClose, onCreated }: {
  invoice: CreditableInvoice
  existingCreditNotes: CreditNote[]
  onClose: () => void
  onCreated: () => void
}) {
  const isPaid = invoice.status === 'paid'

  // `amount_paid_cents > 0` does not imply a card was charged: an offline
  // payment (Record payment) or credits pay with no PaymentIntent. Mirrors
  // domain.Invoice.HasCardPayment.
  const hasCard = hasCardPayment(invoice.stripe_payment_intent_id)

  const [amount, setAmount] = useState('')
  const [refund, setRefund] = useState('')
  const [credit, setCredit] = useState('')
  const [outOfBand, setOutOfBand] = useState('')
  const [reason, setReason] = useState('')
  const [submitError, setSubmitError] = useState('')
  const [submitting, setSubmitting] = useState(false)

  // Each box is read exactly (dollarsToWholeCents) or refused under it:
  // parseFloat read "9,50" as 9 and rounded "10.005" to 10.01. A blank
  // channel box is 0, since the three channels must add up to the total
  // and the sum is shown as they are typed.
  const channelCents = (s: string): number | null => s.trim() === '' ? 0 : dollarsToWholeCents(s)
  const amountParsed = dollarsToWholeCents(amount)
  const refundParsed = channelCents(refund)
  const creditParsed = channelCents(credit)
  const outOfBandParsed = channelCents(outOfBand)
  const amountInputErr = amount.trim() !== '' && amountParsed === null ? amountInputError(amount, '25.00', true) : null
  const channelErr = (s: string, parsed: number | null) => parsed === null ? amountInputError(s, '25.00', true) : null
  const refundErr = channelErr(refund, refundParsed)
  const creditErr = channelErr(credit, creditParsed)
  const outOfBandErr = channelErr(outOfBand, outOfBandParsed)
  const amountCents = amountParsed ?? 0
  const refundCents = refundParsed ?? 0
  const creditCents = creditParsed ?? 0
  const outOfBandCents = outOfBandParsed ?? 0
  const allocatedCents = refundCents + creditCents + outOfBandCents

  // Every ceiling the dialog advertises, derived in one tested place
  // (lib/creditNoteCeilings). The refund ceiling depends on `amountCents`, so
  // it recomputes as the operator types: a refund can never exceed the note it
  // belongs to, and a `max` that ignores that is a promise the field won't keep.
  const {
    alreadyCreditedCents,
    creditableRemainingCents,
    cardRefundableCents,
    refundCeilingCents,
    refundBoundBy,
  } = creditNoteCeilings({
    invoiceTotalCents: invoice.total_amount_cents,
    amountPaidCents: invoice.amount_paid_cents,
    stripePaymentIntentId: invoice.stripe_payment_intent_id,
    creditNotes: existingCreditNotes,
    amountCents,
  })

  // Auto-balance: typing in Refund auto-fills Credit with the
  // remainder so the simple case stays one input + Save. The user
  // can override by editing Credit directly, which sets the manual
  // flag and stops auto-fill.
  const [manualCredit, setManualCredit] = useState(false)
  const [manualOOB, setManualOOB] = useState(false)

  const handleAmountChange = (v: string) => {
    setAmount(v)
    if (!isPaid) return
    // Default on a paid invoice: full amount → credit balance.
    if (!manualCredit && !manualOOB && refund === '') {
      setCredit(v)
    }
  }
  const handleRefundChange = (v: string) => {
    setRefund(v)
    if (!isPaid) return
    if (!manualCredit && !manualOOB) {
      const remainder = amountCents - (channelCents(v) ?? 0) - outOfBandCents
      setCredit(remainder >= 0 ? (remainder / 100).toFixed(2) : '0.00')
    }
  }
  const handleCreditChange = (v: string) => {
    setCredit(v)
    setManualCredit(true)
  }
  const handleOOBChange = (v: string) => {
    setOutOfBand(v)
    setManualOOB(true)
    if (!manualCredit) {
      const remainder = amountCents - refundCents - (channelCents(v) ?? 0)
      setCredit(remainder >= 0 ? (remainder / 100).toFixed(2) : '0.00')
    }
  }

  // Live invariants for the Save gate.
  const allocationMatches = isPaid ? allocatedCents === amountCents : true
  const refundOverCap = refundCents > cardRefundableCents
  const reasonOk = reason.trim().length > 0
  const amountOk = amountParsed !== null && amountParsed > 0
  const channelsOk = !isPaid || (!refundErr && !creditErr && !outOfBandErr)
  const amountOverCreditable = amountCents > creditableRemainingCents
  const canSubmit =
    reasonOk && amountOk && channelsOk && allocationMatches && !refundOverCap && !amountOverCreditable && !submitting

  // One Idempotency-Key per dialog open, sent with every submit from it. A
  // retry after a lost answer (a timeout while Stripe is slow) then gets the
  // first request's credit note back instead of making a second one. The
  // dialog unmounts on close, so the next open makes a new key.
  const [idemKey] = useState(() => crypto.randomUUID())

  // Single-flight: the per-invoice lock only enforces the over-credit CAP, not
  // dedup — a double-fire that still fits under the remaining creditable amount
  // issues a SECOND credit note (a real second card refund / balance credit).
  const onSubmit = useSingleFlight(async (e: React.FormEvent) => {
    e.preventDefault()
    if (!canSubmit) return
    setSubmitting(true)
    setSubmitError('')
    try {
      await api.createCreditNote({
        invoice_id: invoice.id,
        reason: reason.trim(),
        ...(isPaid
          ? {
              refund_amount_cents: refundCents,
              credit_amount_cents: creditCents,
              out_of_band_amount_cents: outOfBandCents,
            }
          : {}),
        auto_issue: true,
        lines: [{ description: reason.trim(), quantity: 1, unit_amount_cents: amountCents }],
      }, idemKey)
      onCreated()
    } catch (err) {
      setSubmitError(err instanceof Error ? err.message : 'Failed to create credit note')
    } finally {
      setSubmitting(false)
    }
  })

  // Every figure in this dialog is an amount of THIS invoice — its currency,
  // not the workspace default.
  const fmt = (cents: number) => formatCents(cents, invoice.currency)

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Issue credit note</DialogTitle>
        </DialogHeader>
        <form onSubmit={onSubmit} noValidate className="space-y-4">
          <div>
            <Label>Invoice</Label>
            <p className="text-sm text-muted-foreground font-mono mt-1">{invoice.invoice_number}</p>
          </div>
          <div className="space-y-2">
            <Label htmlFor="cn-reason">Reason</Label>
            <Input
              id="cn-reason"
              placeholder="e.g. Service disruption, billing error"
              maxLength={500}
              value={reason}
              onChange={(e) => setReason(e.target.value)}
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="cn-amount" className="flex items-center justify-between">
              <span>Amount ({getCurrencySymbol(invoice.currency)})</span>
              <span className="text-xs text-muted-foreground font-normal">
                max {fmt(creditableRemainingCents)}
              </span>
            </Label>
            <Input
              id="cn-amount"
              type="text"
              inputMode="decimal"
              value={amount}
              aria-invalid={amountInputErr ? true : undefined}
              onChange={(e) => handleAmountChange(e.target.value)}
            />
            {amountInputErr && <p className="text-xs text-destructive">{amountInputErr}</p>}
            {amountOverCreditable && (
              <p className="text-xs text-destructive">
                Credit note cannot exceed {fmt(creditableRemainingCents)} still creditable
                {alreadyCreditedCents > 0
                  ? ` (${fmt(alreadyCreditedCents)} of ${fmt(invoice.total_amount_cents)} already credited)`
                  : ''}.
              </p>
            )}
            {!amountOverCreditable && alreadyCreditedCents > 0 && (
              <p className="text-xs text-muted-foreground">
                {fmt(alreadyCreditedCents)} of {fmt(invoice.total_amount_cents)} already credited by
                earlier credit notes.
              </p>
            )}
          </div>

          {isPaid && (
            <div className="space-y-3 rounded-md border bg-muted/30 p-3">
              <div className="text-xs text-muted-foreground">
                Allocate the credit note total across these channels. The three amounts must sum to the credit note total.
              </div>
              <div className="space-y-2">
                <Label htmlFor="cn-refund" className="flex items-center justify-between">
                  <span>Refund to card</span>
                  <span className="text-xs text-muted-foreground font-normal">
                    {hasCard ? `max ${fmt(refundCeilingCents)}` : 'not paid by card'}
                  </span>
                </Label>
                <Input
                  id="cn-refund"
                  type="text"
                  inputMode="decimal"
                  value={refund}
                  aria-invalid={refundErr ? true : undefined}
                  onChange={(e) => handleRefundChange(e.target.value)}
                  disabled={!hasCard}
                />
                {refundErr && <p className="text-xs text-destructive">{refundErr}</p>}
                {!hasCard && (
                  <p className="text-xs text-muted-foreground">
                    This invoice was paid outside Stripe, so there is no card to refund. Use
                    Credit balance or Outside Stripe.
                  </p>
                )}
                {/* Name the rule that actually binds. Deliberately NOT phrased
                    as "$X still refundable to card": when the invoice's
                    remaining creditable amount is the floor, leftover card
                    headroom can never be refunded on this invoice at all, and
                    advertising it as available is the same false-ceiling defect
                    this field just had. */}
                {refundBoundBy === 'note-total' && (
                  <p className="text-xs text-muted-foreground">
                    Limited by this note&apos;s {fmt(amountCents)} total — raise it to refund up to{' '}
                    {fmt(creditableRemainingCents)}, all that is still creditable.
                  </p>
                )}
                {refundBoundBy === 'creditable' && (
                  <p className="text-xs text-muted-foreground">
                    Limited by the invoice, not the card — {fmt(creditableRemainingCents)} is all
                    that is still creditable. The {fmt(cardRefundableCents)} left on the card can no
                    longer be refunded.
                  </p>
                )}
                {hasCard && refundOverCap && (
                  <p className="text-xs text-destructive">
                    Refund cannot exceed {fmt(cardRefundableCents)} paid via card
                    {cardRefundableCents < invoice.amount_paid_cents
                      ? ` (after ${fmt(invoice.amount_paid_cents - cardRefundableCents)} prior refunds)`
                      : ''}
                    .
                  </p>
                )}
              </div>
              <div className="space-y-2">
                <Label htmlFor="cn-credit">Credit balance</Label>
                <Input
                  id="cn-credit"
                  type="text"
                  inputMode="decimal"
                  value={credit}
                  aria-invalid={creditErr ? true : undefined}
                  onChange={(e) => handleCreditChange(e.target.value)}
                />
                {creditErr && <p className="text-xs text-destructive">{creditErr}</p>}
              </div>
              <div className="space-y-2">
                <Label htmlFor="cn-oob">Outside Stripe (cash, ACH, manual)</Label>
                <Input
                  id="cn-oob"
                  type="text"
                  inputMode="decimal"
                  value={outOfBand}
                  aria-invalid={outOfBandErr ? true : undefined}
                  onChange={(e) => handleOOBChange(e.target.value)}
                />
                {outOfBandErr && <p className="text-xs text-destructive">{outOfBandErr}</p>}
              </div>
              <div className="flex items-center justify-between pt-2 border-t text-sm">
                <span className="text-muted-foreground">Allocated</span>
                <span className={allocationMatches ? 'font-medium text-foreground' : 'font-medium text-destructive'}>
                  {fmt(allocatedCents)} / {fmt(amountCents)}
                  {amountCents > 0 && (allocationMatches ? ' ✓' : ' ✗')}
                </span>
              </div>
            </div>
          )}

          {submitError && (
            <p className="text-xs text-destructive">{submitError}</p>
          )}

          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={!canSubmit}>
              {submitting ? <><Loader2 size={14} className="animate-spin mr-2" />Creating...</> : 'Issue credit note'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

