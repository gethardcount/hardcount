package domain

import (
	"context"
	"time"
)

// The charge hold is invoices.auto_charge_claimed_until (ADR-127). It has
// three meanings:
//
//   - NULL: no charge is outstanding.
//   - set, in the future: its owner is working (a charge, or dunning's final
//     action).
//   - set, in the past: a charge may have been sent and its result was not
//     saved — a crash, a caller's deadline, a lost answer.
//
// The rule: nothing takes the hold, charges, or does anything irreversible
// while it is set, except its owner. Only the owner clears its own hold (by
// comparing the value it wrote), and only the stale-hold sweep
// (payment.Reconciler) clears a stale one, after asking Stripe what the lost
// charge did. Write-off is the one action allowed on a stale hold: it moves no
// money, and a payment found later still settles the invoice.
//
// Before ADR-127 an expired hold counted as free, so an interrupted charge
// left the invoice looking exactly like one never charged, and the next
// initiator (another charge kind, a changed card, a key Stripe had pruned, the
// hosted page) took a second payment (#976).

// ChargeHold is one owner's hold on an invoice: the exact
// auto_charge_claimed_until value its claim wrote. It is the owner's token —
// releasing, and the re-check before a charge is sent, compare against it, so
// a late owner can never clear or use a hold another owner has taken since.
// The zero value means the claim was refused.
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
	// ChargeHoldLive: the hold is in the future; its owner is working.
	ChargeHoldLive
	// ChargeHoldStale: the hold has passed with no result saved; only the
	// stale-hold sweep may clear it.
	ChargeHoldStale
)

// The hold's timing. The three numbers together make "Stripe has nothing for
// this attempt" a safe conclusion for the sweep (pinned by
// TestChargeHoldTiming):
//
//   - a claim writes now() + ChargeHoldTTL;
//   - the charge re-checks, right before it sends, that its hold has at least
//     ChargeSendMargin left, and bounds the Stripe call by ChargeSendBudget.
//     ChargeSendBudget <= ChargeSendMargin, so no request for an attempt can
//     start after its hold ends;
//   - the sweep looks only at holds that ended StaleChargeHoldWait ago, so
//     Stripe has had that long to finish anything it received.
const (
	ChargeHoldTTL       = 5 * time.Minute
	ChargeSendMargin    = 2 * time.Minute
	ChargeSendBudget    = 2 * time.Minute
	StaleChargeHoldWait = 10 * time.Minute
)

// StaleChargeHold is one row the stale-hold sweep checks: the invoice, the
// hold value and charge counter it read (the sweep's clear compares both), and
// the customer's Stripe customer id (empty when the customer has none, which
// means no charge can have been sent).
type StaleChargeHold struct {
	Invoice          Invoice
	Hold             time.Time
	StripeCustomerID string
}

type chargeHoldOwnerKey struct{}

type chargeHoldOwner struct {
	invoiceID string
	until     time.Time
}

// WithChargeHoldOwner marks ctx as running under the hold h on invoiceID.
// Dunning's final action takes the hold for the subscription cancel, and the
// cancel can void this same invoice (a wholly unused prebill, ADR-118); the
// void refuses while a hold is set, so without this the cancel would refuse
// its own owner's hold and fail on every attempt.
func WithChargeHoldOwner(ctx context.Context, invoiceID string, h ChargeHold) context.Context {
	if !h.Held() {
		return ctx
	}
	return context.WithValue(ctx, chargeHoldOwnerKey{}, chargeHoldOwner{invoiceID: invoiceID, until: h.Until})
}

// OwnsChargeHold reports whether ctx runs under exactly the hold until on
// invoiceID — the value read under the invoice's row lock.
func OwnsChargeHold(ctx context.Context, invoiceID string, until time.Time) bool {
	o, ok := ctx.Value(chargeHoldOwnerKey{}).(chargeHoldOwner)
	return ok && o.invoiceID == invoiceID && o.until.Equal(until)
}
