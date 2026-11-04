package domain

import (
	"context"
	"time"
)

// The charge hold is invoices.auto_charge_claimed_until (ADR-127). It has
// three meanings:
//
//   - NULL: no charge is outstanding.
//   - Set, in the future: its owner is working (a charge, or dunning's final
//     action).
//   - Set, in the past (stale): a charge may have been sent and its result
//     was not saved. The cause can be a crash, a caller's deadline or a lost
//     answer.
//
// While the hold is set, nothing but its owner takes the hold, charges, or
// does anything irreversible. Only the owner clears its own hold, by
// comparing the value it wrote. Only the stale-hold sweep (payment.Reconciler)
// clears a stale hold, after it asks Stripe what the lost charge did.
// Write-off is the one action allowed on a stale hold: it moves no money, and
// a payment found later still settles the invoice.
//
// A stale hold does not count as free. If it did, an interrupted charge
// would leave the invoice looking exactly like one never charged. The next
// initiator (another charge kind, a changed card, a key Stripe had pruned,
// the hosted page) could then take a second payment (#976).

// ChargeHold is one owner's hold on an invoice: the exact
// auto_charge_claimed_until value its claim wrote. It is the owner's token.
// The release, and the re-check before a charge is sent, compare against it.
// So a late owner can never clear or use a hold that another owner has taken
// since. The zero value means the claim was refused.
type ChargeHold struct {
	Until time.Time
}

// Held reports whether the claim that returned h took the hold.
func (h ChargeHold) Held() bool { return !h.Until.IsZero() }

// ChargeHoldState is what an invoice's hold says when it is read.
type ChargeHoldState int

const (
	// ChargeHoldNone: auto_charge_claimed_until is NULL.
	ChargeHoldNone ChargeHoldState = iota
	// ChargeHoldLive: the hold is in the future, and its owner is working.
	ChargeHoldLive
	// ChargeHoldStale: the hold has passed with no result saved. Only the
	// stale-hold sweep may clear it.
	ChargeHoldStale
)

// The hold's timing, checked by TestChargeHoldTiming. The three numbers
// together make "Stripe has nothing for this attempt" a safe conclusion for
// the stale-hold sweep:
//
//   - A claim writes now() + ChargeHoldTTL.
//   - Right before it sends, the charge re-checks that its hold has at least
//     ChargeSendMargin left, and it bounds the Stripe call by
//     ChargeSendBudget. ChargeSendBudget <= ChargeSendMargin, so no request
//     for an attempt can start after its hold ends.
//   - The sweep looks only at holds that ended StaleChargeHoldWait ago, so
//     Stripe has had that long to finish anything it received.
const (
	ChargeHoldTTL       = 5 * time.Minute
	ChargeSendMargin    = 2 * time.Minute
	ChargeSendBudget    = 2 * time.Minute
	StaleChargeHoldWait = 10 * time.Minute
)

// ParkedNothingWait is how long after its hold a parked charge must be before
// "Stripe shows nothing" puts the invoice back where a charge picks it up. A
// parked charge (Invoice.Parked) is one whose Stripe answer was unclear and
// named no PaymentIntent. A payment that Stripe shows is acted on at the
// first check. Only absence waits.
//
// A park follows a 500, a 409, a network error or a timed-out call. In each
// case Stripe received the request, or may have. Stripe's page on
// advanced error handling (docs.stripe.com/error-low-level) says: "During
// incident recovery, Stripe attempts to reconcile failed operations and send
// webhook events for any objects created during reconciliation". It also
// says: "Limit retries of an unresolved operation to the first 24 hours after
// the original request, using the same key and parameters. If the outcome is
// still unknown after that window, don't retry with either the old key or a
// new one. Retrieve the relevant object, inspect the request in the Dashboard
// and use webhook events to reconcile the outcome. If you can't establish
// what happened, contact Stripe support before repeating the operation."
//
// Hardcount retrieves: the stale-hold sweep lists the customer's
// PaymentIntents. When that list still shows none for the invoice 24 hours
// after the attempt, the invoice is charged again under a new key. This is a
// deliberate departure from Stripe's last sentence. Its cost is that a
// payment Stripe creates even later is flagged as payment.duplicate_charge
// (ADR-127, R10).
//
// The hold is at most ChargeHoldTTL after the attempt began, and the attempt
// ends before the hold does (ChargeSendBudget). So a hold older than this
// wait means at least 24 hours have passed since the attempt.
const ParkedNothingWait = 24 * time.Hour

// StaleChargeHold is one row the stale-hold sweep checks. It carries the
// invoice, and the hold value and charge counter the sweep read. When the
// sweep clears the hold, it compares both. StripeCustomerID is the
// customer's Stripe customer id. It is empty when the customer has none,
// which means no charge can have been sent.
type StaleChargeHold struct {
	Invoice          Invoice
	Hold             time.Time
	StripeCustomerID string
	// PastParkedWait: the hold ended more than ParkedNothingWait ago, read on
	// the database clock the hold was written with. It decides only what
	// "Stripe shows nothing" does to a parked invoice.
	PastParkedWait bool
}

type chargeHoldOwnerKey struct{}

type chargeHoldOwner struct {
	invoiceID string
	until     time.Time
}

// WithChargeHoldOwner marks ctx as running under the hold h on invoiceID.
// Dunning's final action takes the hold for the subscription cancel. The
// cancel can void this same invoice when it is a wholly unused prebill (an
// invoice billed in advance for time not used at all, ADR-118). The void
// refuses while a hold is set. Without this mark, the cancel would refuse its
// own owner's hold and fail on every attempt.
func WithChargeHoldOwner(ctx context.Context, invoiceID string, h ChargeHold) context.Context {
	if !h.Held() {
		return ctx
	}
	return context.WithValue(ctx, chargeHoldOwnerKey{}, chargeHoldOwner{invoiceID: invoiceID, until: h.Until})
}

// OwnsChargeHold reports whether ctx runs under exactly the hold until on
// invoiceID, the value read under the invoice's row lock.
func OwnsChargeHold(ctx context.Context, invoiceID string, until time.Time) bool {
	o, ok := ctx.Value(chargeHoldOwnerKey{}).(chargeHoldOwner)
	return ok && o.invoiceID == invoiceID && o.until.Equal(until)
}
