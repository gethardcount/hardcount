package domain

// CreditBalanceFilter picks balances by how much is left: the `balance`
// filter of GET /v1/credits/balances.
type CreditBalanceFilter string

const (
	// CreditBalanceOut is a balance at zero or less.
	CreditBalanceOut CreditBalanceFilter = "out"
	// CreditBalanceLow is a balance above zero and below the tenant's
	// credit_balance_low_threshold_cents. With no threshold set, no balance
	// is low.
	CreditBalanceLow CreditBalanceFilter = "low"
)

// CreditBalanceSQL is a customer's balance in one currency: the sum of its
// ledger rows, over customer_credit_ledger grouped by customer_id and
// currency (ADR-129).
const CreditBalanceSQL = `COALESCE(SUM(amount_cents), 0)`

// CreditBalanceFilterSQL is the HAVING predicate for f over that grouping, or
// "" for no filter. The credit balances list and the dashboard's counts share
// it, so a count and its list never disagree. It reads tenant_settings and
// needs a tenant transaction.
func CreditBalanceFilterSQL(f CreditBalanceFilter) string {
	switch f {
	case CreditBalanceOut:
		return CreditBalanceSQL + ` <= 0`
	case CreditBalanceLow:
		// A NULL threshold matches nothing.
		return CreditBalanceSQL + ` > 0 AND ` + CreditBalanceSQL + ` < (
			SELECT credit_balance_low_threshold_cents FROM tenant_settings
			WHERE tenant_id = current_setting('app.tenant_id', true))`
	}
	return ""
}
