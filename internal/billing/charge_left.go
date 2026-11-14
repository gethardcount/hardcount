package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/payment"
)

// chargeLeft is what is left of an invoice's collection after a run that did
// not charge it, read from the invoice as it is stored now.
type chargeLeft int

const (
	// nothingLeft: no later run charges it. It is no longer finalized and
	// pending (paid, voided, failed into dunning), or it left the queue.
	nothingLeft chargeLeft = iota
	// claimableAgain: it is queued, pending, with no hold, so a later run
	// claims and charges it.
	claimableAgain
	// attemptUnresolved: a charge attempt holds it, or the result of one is
	// not known (unknown, processing). The payment reconciler's stale-hold and
	// unknown sweeps resolve it on a billing run; a later run then charges
	// what is left.
	attemptUnresolved
	// leftUnreadable: the invoice or its hold could not be read.
	leftUnreadable
)

// chargeLeftOf answers chargeLeft from the invoice and its hold as stored.
// It is the claim's own predicate (ClaimAutoCharge), read rather than taken.
func chargeLeftOf(inv domain.Invoice, hold domain.ChargeHoldState) chargeLeft {
	switch {
	case inv.Status != domain.InvoiceFinalized:
		return nothingLeft
	case inv.PaymentStatus == domain.PaymentUnknown, inv.PaymentStatus == domain.PaymentProcessing:
		return attemptUnresolved
	case inv.PaymentStatus != domain.PaymentPending || !inv.AutoChargePending:
		return nothingLeft
	case hold != domain.ChargeHoldNone:
		return attemptUnresolved
	default:
		return claimableAgain
	}
}

// chargeNotMade is a charge a run did not make, and what is left of it. It is
// domain.ErrLeftForNextRun, joined with why the charge was not made. Its
// operator message is what a test clock's failure reason shows: what
// happened, and what to do.
type chargeNotMade struct {
	invoice    string // as the operator knows it: its number, else its id
	why        string // what stopped the charge, in plain words
	keyRefused bool   // Stripe refused the account's API key, or none is connected
	left       chargeLeft
	cause      error
}

func (e *chargeNotMade) OperatorSafeMessage() string {
	switch {
	case e.left == attemptUnresolved && e.keyRefused:
		return fmt.Sprintf("Invoice %s: Stripe refused this account's API key while charging it, so the result of the charge is not known yet. Fix the key under Settings → Payments; Hardcount then reads the result from Stripe on a later billing run, and Retry advance charges what is left.", e.invoice)
	case e.left == attemptUnresolved:
		return fmt.Sprintf("Invoice %s: an earlier charge attempt is not resolved yet. Hardcount reads its result from Stripe on a later billing run; Retry advance after that.", e.invoice)
	case e.left == leftUnreadable:
		return fmt.Sprintf("Invoice %s was not charged: %s, and its state could not be read just then. Retry advance.", e.invoice, e.why)
	case e.keyRefused:
		return fmt.Sprintf("Invoice %s was not charged: Stripe refused this account's API key, or none is connected. Nothing was charged. Fix the key under Settings → Payments, then Retry advance.", e.invoice)
	default:
		return fmt.Sprintf("Invoice %s was not charged: %s. Nothing was charged. Retry advance to charge it.", e.invoice, e.why)
	}
}

// nextStep is what happens next, for the logs of a run that goes on.
func (e *chargeNotMade) nextStep() string {
	switch {
	case e.left == attemptUnresolved && e.keyRefused:
		return "the result of the charge is not known; the payment reconciler reads it from Stripe once the key is fixed under Settings → Payments"
	case e.left == attemptUnresolved:
		return "an earlier charge attempt is not resolved; the payment reconciler reads its result from Stripe on a later billing run"
	case e.left == leftUnreadable:
		return "its state could not be read; a later auto-charge run looks again"
	case e.keyRefused:
		return "the invoice is still queued; charges wait until the Stripe key is fixed under Settings → Payments"
	default:
		return "the invoice is still queued, and a later auto-charge run charges it"
	}
}

func (e *chargeNotMade) Error() string {
	return e.OperatorSafeMessage() + " (" + e.cause.Error() + ")"
}

func (e *chargeNotMade) Unwrap() []error { return []error{domain.ErrLeftForNextRun, e.cause} }

// chargeNotMadeFor is what a run that did not charge inv returns: nil when
// nothing is left (the invoice was settled meanwhile, say), else a
// chargeNotMade. It reads the invoice and its hold as stored now, since a
// charger can keep its hold or fail to release it, so the error that stopped
// the charge does not say on its own whether a later run can claim it.
func (e *Engine) chargeNotMadeFor(ctx context.Context, inv domain.Invoice, why string, cause error) error {
	var ke *payment.StripeKeyError
	notMade := &chargeNotMade{invoice: inv.InvoiceNumber, why: why, keyRefused: errors.As(cause, &ke), left: leftUnreadable, cause: cause}
	if notMade.invoice == "" {
		notMade.invoice = inv.ID
	}
	fresh, err := e.invoices.GetInvoice(ctx, inv.TenantID, inv.ID)
	if err == nil {
		var hold domain.ChargeHoldState
		if hold, err = e.invoices.ChargeHoldStateOf(ctx, inv.TenantID, inv.ID); err == nil {
			notMade.left = chargeLeftOf(fresh, hold)
		}
	}
	if err != nil {
		slog.WarnContext(ctx, "auto-charge: could not read what is left of a charge that was not made", "invoice_id", inv.ID, "error", err)
	}
	if notMade.left == nothingLeft {
		slog.InfoContext(ctx, "auto-charge: not charged, and nothing is left to charge", "invoice_id", inv.ID, "error", cause)
		return nil
	}
	return notMade
}
