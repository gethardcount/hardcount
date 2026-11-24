package domain

// CreditBalanceFilter picks balances by how much is left: the `balance`
// filter of GET /v1/credits/balances. Both values list the balances that
// need the operator, so they leave out archived customers.
type CreditBalanceFilter string

const (
	// CreditBalanceOut is a balance at zero or less.
	CreditBalanceOut CreditBalanceFilter = "out"
	// CreditBalanceLow is a balance above zero and below the tenant's
	// credit_balance_low_threshold_cents. With no threshold set, no balance
	// is low.
	CreditBalanceLow CreditBalanceFilter = "low"
)
