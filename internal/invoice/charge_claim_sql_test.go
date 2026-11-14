package invoice

import (
	"strings"
	"testing"
)

// A claim's UPDATE adds nothing to the predicate it is given: its WHERE is
// the invoice and that predicate. ChargeClaimStateOf reads ClaimAutoCharge's
// predicate, autoChargeClaim, so a condition added here and not to the list
// would refuse claims the read calls claimable.
func TestChargeClaimSQL_WhereIsThePredicateAlone(t *testing.T) {
	const pred = "<the claim's predicate>"
	q := chargeClaimSQL(pred)
	start, end := strings.Index(q, "WHERE "), strings.Index(q, "RETURNING")
	if start < 0 || end < start {
		t.Fatalf("no WHERE ... RETURNING in:\n%s", q)
	}
	if got, want := strings.TrimSpace(q[start+len("WHERE "):end]), "id = $1 AND ("+pred+")"; got != want {
		t.Errorf("the claim's WHERE is %q, want %q", got, want)
	}
}

// The claim and the read are one list: the WHERE has every condition, and the
// read has a branch for each, in order, with its answer.
func TestAutoChargeClaim_ClaimAndReadAreOneList(t *testing.T) {
	where, state := autoChargeClaim.where(), autoChargeClaim.state()
	at := 0
	for _, c := range autoChargeClaim {
		if !strings.Contains(where, "("+c.sql+")") {
			t.Errorf("the claim's WHERE lacks %q", c.sql)
		}
		i := strings.Index(state[at:], "WHEN ("+c.sql+") IS NOT TRUE THEN ")
		if i < 0 {
			t.Fatalf("the read has no branch for %q after the ones before it", c.sql)
		}
		at += i + 1
	}
}
