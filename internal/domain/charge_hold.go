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
//   - Set, in the past (stale): its charger is gone (a crash, a caller's
//     deadline, a lost answer). On an invoice still pending or failed that
//     charge never confirmed a payment: a charge puts its PaymentIntent on
//     the invoice, as an unknown outcome, before it confirms (ADR-137).
//
// While the hold is set, nothing but its owner takes the hold, charges, or
// does anything irreversible. The owner clears its own hold, by comparing
// the value it wrote. A settle clears it too. One that marks the invoice paid
// clears any hold: nothing can charge a paid invoice, and a charger that has
// claimed and not yet stamped finds its stamp refused. One that marks it
// failed clears the hold only when the invoice was in flight (unknown or
// processing) on that PaymentIntent and the hold carries its token: no claim
// admits an in-flight invoice, so that hold is the attempt's own, and a hold
// without its token is an earlier version's, kept for a person. Otherwise
// only the stale-hold sweep (payment.Reconciler) clears a stale hold; it
// needs no answer from Stripe to do so.
// Write-off is the one action allowed on a stale hold: it moves no money.
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
	// ChargeHoldStale: the hold has passed with no result saved. The
	// stale-hold sweep clears it, or the settle that records the result.
	ChargeHoldStale
)

// The hold's timing, checked by TestChargeHoldTiming. The three numbers
// together keep a sweep from acting on a charge that has yet to send:
//
//   - A claim writes now() + ChargeHoldTTL.
//   - Right before it sends, the charge re-checks that its hold has at least
//     ChargeSendMargin left, and it bounds the Stripe call by
//     ChargeSendBudget. ChargeSendBudget <= ChargeSendMargin, so no request
//     for an attempt can start after its hold ends.
//   - The stale-hold sweep looks only at holds that ended
//     StaleChargeHoldWait ago, and the unknown sweep only at invoices whose
//     hold has ended. Neither needs the wait for safety: the stamp and the
//     clear are compare-and-sets on one row, and Stripe refuses a confirm on a
//     canceled PaymentIntent (ADR-137). StaleChargeHoldWait is kept from the
//     one-call charge, where it gave the charge's webhook time to arrive
//     before the sweep asked Stripe. Today it only lets a slow charger's own
//     release land first, and delays the next charge of a stopped one by up
//     to ten minutes; removing it is a follow-up.
const (
	ChargeHoldTTL       = 5 * time.Minute
	ChargeSendMargin    = 2 * time.Minute
	ChargeSendBudget    = 2 * time.Minute
	StaleChargeHoldWait = 10 * time.Minute
)

// StaleChargeHold is one row the stale-hold sweep checks. It carries the
// invoice, and the hold value and charge counter the sweep read. When the
// sweep clears the hold, it compares both.
type StaleChargeHold struct {
	Invoice Invoice
	Hold    time.Time
	// Tokened: the hold carries its token (charge_hold_token equal to it),
	// so it was taken by a charger that puts its PaymentIntent on the
	// invoice before confirming (ADR-137). A hold without it was taken by an
	// earlier version, which charged in one call and saved nothing first.
	Tokened bool
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
