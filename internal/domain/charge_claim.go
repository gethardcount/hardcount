package domain

// ChargeClaimState is whether an auto-charge claim of an invoice would be
// taken now, and if not, why not. It is the claim's own rule
// (invoice.PostgresStore.ClaimAutoCharge), read rather than taken
// (ChargeClaimStateOf), so a run that did not charge an invoice can say what
// is left of it.
type ChargeClaimState int

const (
	// ChargeClaimable: a claim would be taken now.
	ChargeClaimable ChargeClaimState = iota
	// ChargeNothingToCharge: the invoice is not finalized, not pending, or
	// not in the auto-charge queue (paid, voided, failed into dunning, or
	// gone).
	ChargeNothingToCharge
	// ChargeCollectionPaused: its subscription's collection is paused at the
	// invoice's own now.
	ChargeCollectionPaused
	// ChargePayingOnHostedPage: the customer may be paying it on the hosted
	// page (HostedCheckoutLiveSQL).
	ChargePayingOnHostedPage
	// ChargeHeldByAttempt: a charge attempt holds it, or the result of one is
	// not known yet (payment unknown or processing).
	ChargeHeldByAttempt
)
