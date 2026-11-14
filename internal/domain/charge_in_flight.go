package domain

// A charge of an open invoice is in flight while its payment's result is not
// known yet (unknown or processing), while a charge attempt holds it (a
// charge hold, live or stale), or while the customer may be paying it on the
// hosted page. Every charge claim refuses such an invoice, and both dunning
// due lists leave its run out. One definition, so they cannot drift apart:
// each condition below holds when its reason does NOT apply. alias names the
// invoices row in the query, "invoices" when the table has no alias; it is
// never empty, since the hosted-page condition's subquery has an id of its
// own that a bare id would mean.

// ChargeCondition is one condition a charge needs, and what a read of the
// claim answers when it is the first that does not hold.
type ChargeCondition struct {
	SQL     string
	IfFalse ChargeClaimState
}

// PaymentNotInFlightSQL holds unless the payment's result is not known yet.
func PaymentNotInFlightSQL(alias string) string {
	return column(alias, "payment_status") + " NOT IN ('unknown', 'processing')"
}

// ChargeHoldFreeSQL holds when no charge hold is set. A stale hold is not
// free: a charge may have been sent and its result not saved (ADR-127).
func ChargeHoldFreeSQL(alias string) string {
	return column(alias, "auto_charge_claimed_until") + " IS NULL"
}

// NoLiveHostedPageSQL holds unless the customer may be paying on the hosted
// page (HostedCheckoutLiveSQL): one payer at a time.
func NoLiveHostedPageSQL(alias string) string {
	return "NOT " + HostedCheckoutLiveSQL(column(alias, "id"))
}

// ChargeNotInFlight is the three conditions, in the order a read of a claim
// names the first that fails.
func ChargeNotInFlight(alias string) []ChargeCondition {
	return []ChargeCondition{
		{PaymentNotInFlightSQL(alias), ChargeHeldByAttempt},
		{ChargeHoldFreeSQL(alias), ChargeHeldByAttempt},
		{NoLiveHostedPageSQL(alias), ChargePayingOnHostedPage},
	}
}

// ChargeNotInFlightSQL is the three conditions' AND.
func ChargeNotInFlightSQL(alias string) string {
	s := ""
	for i, c := range ChargeNotInFlight(alias) {
		if i > 0 {
			s += " AND "
		}
		s += "(" + c.SQL + ")"
	}
	return "(" + s + ")"
}

func column(alias, name string) string { return alias + "." + name }
