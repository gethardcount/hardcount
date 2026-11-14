package domain

import "testing"

// The three conditions, their answers and their order are one definition;
// ChargeNotInFlightSQL is their AND, qualified by the alias.
func TestChargeNotInFlight_IsOneList(t *testing.T) {
	got := ChargeNotInFlight("i")
	want := []ChargeCondition{
		{"i.payment_status NOT IN ('unknown', 'processing')", ChargeHeldByAttempt},
		{"i.auto_charge_claimed_until IS NULL", ChargeHeldByAttempt},
		{"NOT " + HostedCheckoutLiveSQL("i.id"), ChargePayingOnHostedPage},
	}
	if len(got) != len(want) {
		t.Fatalf("%d conditions, want %d", len(got), len(want))
	}
	and := "("
	for n, c := range want {
		if got[n] != c {
			t.Errorf("condition %d = %+v, want %+v", n, got[n], c)
		}
		if n > 0 {
			and += " AND "
		}
		and += "(" + c.SQL + ")"
	}
	if s := ChargeNotInFlightSQL("i"); s != and+")" {
		t.Errorf("ChargeNotInFlightSQL = %q, want %q", s, and+")")
	}
	if s := NoLiveHostedPageSQL("invoices"); s != "NOT "+HostedCheckoutLiveSQL("invoices.id") {
		t.Errorf("the hosted page names the invoice by its table: %q", s)
	}
}
