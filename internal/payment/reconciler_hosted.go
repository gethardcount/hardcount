package payment

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/sagarsuperuser/velox/internal/auth"
)

// HostedSessionLedger is the slice of the checkout-session ledger the hosted
// sweep needs. *CheckoutSessionStore implements it.
type HostedSessionLedger interface {
	ListHostedToReconcile(ctx context.Context, checkedBefore time.Time, limit int) ([]CheckoutClaim, error)
	MarkSessionCompleted(ctx context.Context, tenantID, claimID string) error
	MarkExpired(ctx context.Context, tenantID, claimID string) error
	TouchReconcile(ctx context.Context, tenantID, claimID string) error
}

// CheckoutSessionReader reads a Checkout Session's outcome at Stripe.
// *LiveStripeClient implements it.
type CheckoutSessionReader interface {
	GetCheckoutSession(ctx context.Context, sessionID string) (CheckoutSessionResult, error)
}

// CheckoutSessionResult is the part of a Stripe Checkout Session the sweep
// acts on.
type CheckoutSessionResult struct {
	Status          string // open | complete | expired
	PaymentStatus   string // paid | unpaid | no_payment_required
	PaymentIntentID string
	CustomerID      string // the Stripe customer the session saved the card to
}

// CheckoutCardRecorder records the card a finished Checkout session saved as
// the customer's payment method. *Stripe implements it (RecordCheckoutCard),
// shared with the checkout.session.completed webhook.
type CheckoutCardRecorder interface {
	RecordCheckoutCard(ctx context.Context, tenantID, customerID, stripeCustomerID, purpose string) error
}

// hostedSweepGrace is how long a session must sit unchecked (since it was
// created, filled or last looked at) before the sweep asks Stripe about it,
// so the payment_intent.succeeded webhook records an ordinary payment first.
const hostedSweepGrace = 5 * time.Minute

// SetHostedSessions wires the hosted-page sweep. Unwired (tests, Stripe not
// configured), the sweep does nothing.
func (r *Reconciler) SetHostedSessions(ledger HostedSessionLedger, reader CheckoutSessionReader, cards CheckoutCardRecorder) {
	r.hostedLedger = ledger
	r.hostedReader = reader
	r.hostedCards = cards
}

// sweepHostedSessions asks Stripe how each unresolved hosted Checkout
// session stands. It exists for the payment no webhook reported: the hosted
// PI is not on the invoice until it settles, so the PI sweeps above can never
// find it, and the invoice would stay unpaid (and blocked from Velox charges)
// until someone noticed.
//
//   - complete and paid -> settle through the shared primitive. That records
//     the payment, or, when the invoice was paid by another PaymentIntent in
//     the meantime, raises payment.duplicate_charge (or
//     payment.received_on_voided_invoice) for the operator to refund. Then
//     record the card the session saved (ADR-021), which otherwise only the
//     lost checkout.session.completed webhook would have done.
//   - expired -> record it; nothing was paid.
//   - anything else, or an error -> touch the row so the queue rotates, and
//     look again next tick.
//
// It calls SettleSucceeded directly rather than r.settle, whose "already
// paid, skip" shortcut would swallow exactly the duplicate this sweep exists
// to surface.
func (r *Reconciler) sweepHostedSessions(ctx context.Context, limit int) (int, []error) {
	if r.hostedLedger == nil || r.hostedReader == nil || r.settler == nil {
		return 0, nil
	}
	rows, err := r.hostedLedger.ListHostedToReconcile(ctx, r.now().Add(-hostedSweepGrace), limit)
	if err != nil {
		return 0, []error{fmt.Errorf("list hosted sessions: %w", err)}
	}
	var errs []error
	resolved := 0
	for _, c := range rows {
		done, err := r.reconcileHostedSession(ctx, c)
		if err != nil {
			errs = append(errs, fmt.Errorf("checkout session %s: %w", c.ID, err))
		}
		if done {
			resolved++
			continue
		}
		if tErr := r.hostedLedger.TouchReconcile(ctx, c.TenantID, c.ID); tErr != nil {
			slog.WarnContext(ctx, "reconcile: could not rotate hosted session",
				"claim_id", c.ID, "error", tErr)
		}
	}
	return resolved, errs
}

func (r *Reconciler) reconcileHostedSession(ctx context.Context, c CheckoutClaim) (bool, error) {
	ctx = auth.WithTenantID(ctx, c.TenantID)

	sess, err := r.hostedReader.GetCheckoutSession(ctx, c.StripeSessionID)
	if err != nil {
		return false, fmt.Errorf("get checkout session: %w", err)
	}
	switch sess.Status {
	case "expired":
		if err := r.hostedLedger.MarkExpired(ctx, c.TenantID, c.ID); err != nil {
			return false, fmt.Errorf("mark expired: %w", err)
		}
		return true, nil
	case "complete":
		if sess.PaymentStatus != "paid" || sess.PaymentIntentID == "" {
			// Cards are the only method offered, so a complete session is
			// paid. Anything else is unexpected: leave it for a person.
			slog.WarnContext(ctx, "reconcile: hosted session complete but not paid",
				"claim_id", c.ID, "stripe_session_id", c.StripeSessionID,
				"payment_status", sess.PaymentStatus)
			return false, nil
		}
		pi, err := r.client.GetPaymentIntent(ctx, sess.PaymentIntentID)
		if err != nil {
			return false, fmt.Errorf("get payment intent %s: %w", sess.PaymentIntentID, err)
		}
		inv, err := r.invoices.Get(ctx, c.TenantID, c.InvoiceID)
		if err != nil {
			return false, fmt.Errorf("get invoice %s: %w", c.InvoiceID, err)
		}
		// The webhook already reported this payment as an anomaly (a duplicate,
		// or money on a voided invoice): settling again would send the
		// operator's systems a second payment.duplicate_charge for one charge.
		if inv.PaymentAnomalyPaymentIntentID != pi.ID {
			if err := r.settler.SettleSucceeded(ctx, c.TenantID, inv, pi.ID, pi.AmountReceivedCents, SourceReconciler); err != nil {
				return false, fmt.Errorf("settle hosted payment: %w", err)
			}
		}
		// Before marking the row completed: a failed card record keeps the row
		// unresolved, so the next tick tries again (the settle above is then a
		// no-op on the paid invoice).
		if r.hostedCards != nil && sess.CustomerID != "" {
			if err := r.hostedCards.RecordCheckoutCard(ctx, c.TenantID, inv.CustomerID, sess.CustomerID, PurposeHostedInvoicePay); err != nil {
				return false, fmt.Errorf("record the session's card: %w", err)
			}
		}
		if err := r.hostedLedger.MarkSessionCompleted(ctx, c.TenantID, c.ID); err != nil {
			// Settled; the row comes back next tick and settles as a no-op.
			return true, fmt.Errorf("mark completed: %w", err)
		}
		return true, nil
	default:
		return false, nil
	}
}
