package domain

// HostedCheckoutLiveSQL is the "one payer at a time" rule as a SQL boolean:
// true while the invoice has a hosted Checkout page that may still take the
// customer's money, or a hosted payment Velox has not recorded yet. While it
// holds, no Velox charge may start (the charge claims in invoice/postgres.go)
// and the dunning due-run scans skip the run, since its claim cannot succeed
// (dunning/postgres.go). One fragment, so the claims and the scans cannot
// disagree about when a charge is allowed. invoiceID is the SQL expression
// naming the invoice (e.g. "invoices.id", "i.id").
//
// A row blocks while:
//   - it is an unfilled claim (no Stripe session recorded yet): its link may
//     be about to reach the customer, so for the session's life, 1 hour, plus
//     5 minutes;
//   - it is a recorded session still 'open' or 'superseded': until the hosted
//     sweep (payment/reconciler_hosted.go) asks Stripe how it ended and
//     records that, so a payment nobody reported cannot slip past a lapsed
//     timer;
//   - it is 'completed': the customer paid, and until that payment is recorded
//     the invoice still looks unpaid. The claims require an unpaid invoice, so
//     the block ends when the payment settles.
//
// Recorded sessions stop blocking 24 hours after their expiry regardless, so a
// session the sweep cannot resolve (a revoked key, a persistent Stripe error)
// delays collection by a day instead of stopping it. The sweep logs every
// failure it hits.
func HostedCheckoutLiveSQL(invoiceID string) string {
	return `EXISTS (
		SELECT 1 FROM checkout_sessions cs
		WHERE cs.invoice_id = ` + invoiceID + `
		  AND (
		    (cs.stripe_session_id IS NULL AND cs.status = 'open'
		      AND cs.created_at + interval '65 minutes' > now())
		    OR (cs.stripe_session_id IS NOT NULL
		      AND cs.status IN ('open', 'superseded', 'completed')
		      AND COALESCE(cs.expires_at, cs.created_at + interval '1 hour') + interval '24 hours' > now())
		  )
	)`
}
