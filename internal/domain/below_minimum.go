package domain

import "github.com/gethardcount/hardcount/internal/errs"

// BelowMinimumCode names one fact everywhere it shows: Stripe refused to
// charge an invoice because its amount is below Stripe's minimum charge.
// Stripe's error code for it is amount_too_small. The minimum is $0.50 for
// USD, and a per-currency amount for the rest
// (https://docs.stripe.com/currencies#minimum-and-maximum-charge-amounts).
// It is the error code that Collect payment and the hosted page's Pay answer
// with. It is also the invoice's attention reason, the hosted page's
// pay_unavailable_reason, and the resolution of a payment recovery that ended
// on it.
const BelowMinimumCode = "amount_below_minimum"

// ErrBelowMinimumCharge is the refusal itself. It is an InvalidState error
// with BelowMinimumCode, so the API answers 409 with that code.
// payment.BelowMinimumError wraps it with Stripe's sentence.
//
// The refusal is not a decline. No card was refused, nothing was charged, and
// no card or retry can collect this amount through Stripe. So the invoice is
// not marked failed, and no payment recovery starts (one already running
// ends). The customer gets no failure email. The operator decides, as in
// Lago's model: void the invoice, record a payment made another way, or add
// the amount to the customer's next invoice by hand.
//
// Hardcount learns the minimum from Stripe's answer, never from a table of its
// own. Stripe applies the minimum of the account's settlement currency when
// the charge needs converting. So a fixed table is exact only when the charge
// currency matches the currency of a bank account.
var ErrBelowMinimumCharge = errs.InvalidState("the amount is below Stripe's minimum charge, so it can't be charged by card").WithCode(BelowMinimumCode)

// BelowStripeMinimum reports whether Stripe's latest answer to a charge of
// this open invoice was ErrBelowMinimumCharge: the invoice is finalized and
// unpaid, and below_minimum_at is set. Every reader of the fact checks it:
// the attention notice, the hosted page's Pay, and dunning. Dunning ends a
// run on such an invoice at its next tick, since the resolve that follows the
// stamp is a separate call that can be lost.
//
// The stamp is Stripe's answer, not a permanent fact. Stripe applies the
// minimum of the account's settlement currency to a converted charge, so a
// change to the account or to the exchange rate can change the answer. The
// next charge outcome Stripe gives clears the stamp (StampChargeOutcome,
// MarkPaymentFailedReportingTransition).
func (inv Invoice) BelowStripeMinimum() bool {
	return inv.Status == InvoiceFinalized && inv.PaymentStatus == PaymentPending && inv.BelowMinimumAt != nil
}
