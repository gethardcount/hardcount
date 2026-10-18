package payment

import "time"

// The interrupted-charge check (ADR-127). Scaffolding: the sweep lands with
// the fix.

const (
	// chargeSendBudget bounds a charge from its record write to its last
	// Stripe request.
	chargeSendBudget = 2 * time.Minute
	// interruptedChargeMinAge is how old a charge record must be before the
	// reconciler asks Stripe about it.
	interruptedChargeMinAge = 10 * time.Minute
	// interruptedChargeGiveUpAge is how long a record waits for a successful
	// read of Stripe before the invoice parks.
	interruptedChargeGiveUpAge = 24 * time.Hour
)
