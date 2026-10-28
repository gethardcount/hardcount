package domain

import "github.com/sagarsuperuser/velox/internal/errs"

// BelowMinimumCode names one fact everywhere it shows: Stripe refused to
// charge an invoice because its amount is below Stripe's minimum charge
// (Stripe's error code amount_too_small; $0.50 for USD, and a per-currency
// amount for the rest, https://docs.stripe.com/currencies#minimum-and-maximum-charge-amounts).
// It is the error code Collect payment and the hosted page's Pay answer with,
// the invoice's attention reason, the hosted page's pay_unavailable_reason,
// and the resolution of a payment recovery that ended on it.
const BelowMinimumCode = "amount_below_minimum"

// ErrBelowMinimumCharge is the refusal itself. It is not a decline: no card
// was refused and nothing was charged, and no card or retry can collect this
// amount through Stripe. So the invoice is not marked failed, no payment
// recovery starts (one already running ends), the customer gets no failure
// email, and the operator decides: void the invoice, record a payment made
// another way, or add the amount to the customer's next invoice by hand
// (Lago's model; the min-charge check of 2026-10-22).
//
// Hardcount learns the minimum from Stripe's answer, never from a table of its
// own: Stripe applies the minimum of the account's settlement currency when
// the charge needs converting, so a fixed table is exact only when the
// charge currency matches a bank account's.
//
// An InvalidState with BelowMinimumCode, so the API answers 409 with that
// code. payment.BelowMinimumError wraps it with Stripe's sentence.
var ErrBelowMinimumCharge = errs.InvalidState("the amount is below Stripe's minimum charge, so it can't be charged by card").WithCode(BelowMinimumCode)

// BelowStripeMinimum reports whether Stripe's latest answer to a charge of
// this open invoice was ErrBelowMinimumCharge: the invoice is finalized and
// unpaid, and below_minimum_at is set. It is what every reader of the fact
// checks: the attention notice, the hosted page's Pay, and dunning, which
// ends a run on such an invoice at its next tick (the resolve that follows
// the stamp is a separate call that can be lost). The stamp is Stripe's
// answer, not a permanent fact: Stripe applies the minimum of the account's
// settlement currency to a converted charge, so a change to the account, or
// the exchange rate, can change it. The next charge outcome Stripe gives
// clears it (StampChargeOutcome, MarkPaymentFailedReportingTransition).
func (inv Invoice) BelowStripeMinimum() bool {
	return inv.Status == InvoiceFinalized && inv.PaymentStatus == PaymentPending && inv.BelowMinimumAt != nil
}
