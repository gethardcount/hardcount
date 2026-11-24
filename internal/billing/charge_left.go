package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/payment"
)

// errChargeClaimRefused is why a run that found another charge of the
// invoice running did not charge it.
var errChargeClaimRefused = errors.New("another charge of the invoice was running")

// chargeNotMade is a charge this run did not make and a later run makes:
// domain.ErrLeftForNextRun, joined with why. Its operator message, in the
// run's log, says what happened and what to do.
type chargeNotMade struct {
	invoice    string // as the operator knows it: its number, else its id
	why        string // what stopped the charge, in plain words
	keyRefused bool   // Stripe refused the account's API key, or none is connected
	// held: an earlier charge attempt has not finished
	// (domain.ChargeHeldByAttempt). Otherwise a later run can charge it now.
	held  bool
	cause error
}

func (e *chargeNotMade) OperatorSafeMessage() string {
	switch {
	case e.held && e.keyRefused:
		return fmt.Sprintf("Invoice %s: Stripe refused this account's API key while charging it, so the result of that charge is not known, and the invoice is not charged again until it is: it cannot be charged twice. Fix the key under Settings → Payments; Hardcount then reads the result on a billing run, within the hour, and a later run charges what is left.", e.invoice)
	case e.held:
		return fmt.Sprintf("Invoice %s: an earlier charge attempt has not finished, so it was not charged again: it cannot be charged twice. Hardcount checks that attempt on a billing run, within the hour, and a later run charges what is left.", e.invoice)
	case e.keyRefused:
		return fmt.Sprintf("Invoice %s was not charged: Stripe refused this account's API key, or none is connected. Nothing was charged. Fix the key under Settings → Payments, and a later run charges it.", e.invoice)
	default:
		return fmt.Sprintf("Invoice %s was not charged: %s. Nothing was charged. A later run charges it.", e.invoice, e.why)
	}
}

func (e *chargeNotMade) Error() string {
	return e.OperatorSafeMessage() + " (" + e.cause.Error() + ")"
}

func (e *chargeNotMade) Unwrap() []error { return []error{domain.ErrLeftForNextRun, e.cause} }

// chargeNotMadeFor is what a run that did not charge inv returns, the same at
// every exit that sends no charge: the refused claim, credits that could not
// be applied, a transient answer. The invoice's store answers whether its
// claim would be taken now, and if not, why not (ChargeClaimStateOf, the
// claim's own SQL), since a charger can keep its hold or fail to release it,
// so the error that stopped the charge does not say on its own what is left.
//   - Nothing to charge, collection paused, or the customer paying on the
//     hosted page: nil. Nothing is this job's to do.
//   - An earlier charge attempt has not finished: a chargeNotMade, held.
//   - Claimable after all (the other attempt let go in between, or this run
//     let go): a chargeNotMade.
//
// A failed read is returned as a failed read, wrapping cause too.
func (e *Engine) chargeNotMadeFor(ctx context.Context, inv domain.Invoice, why string, cause error) error {
	number := inv.InvoiceNumber
	if number == "" {
		number = inv.ID
	}
	state, err := e.invoices.ChargeClaimStateOf(ctx, inv.TenantID, inv.ID)
	if err != nil {
		return fmt.Errorf("invoice %s was not charged: %s (%w), and what is left of it could not be read: %w", number, why, cause, err)
	}
	switch state {
	case domain.ChargeClaimable, domain.ChargeHeldByAttempt:
		var ke *payment.StripeKeyError
		return &chargeNotMade{invoice: number, why: why, keyRefused: errors.As(cause, &ke),
			held: state == domain.ChargeHeldByAttempt, cause: cause}
	default:
		if !errors.Is(cause, errChargeClaimRefused) {
			slog.InfoContext(ctx, "auto-charge: not charged, and nothing is left for this run to charge",
				"invoice_id", inv.ID, "claim_state", state, "error", cause)
		}
		return nil
	}
}

// refusedClaim reports whether err is what a refused claim returned. A run
// logs it at Debug: two collectors met on one invoice (the finalize-time collect and the sweep, say), and the one
// holding it charges.
func refusedClaim(err error) bool { return errors.Is(err, errChargeClaimRefused) }
