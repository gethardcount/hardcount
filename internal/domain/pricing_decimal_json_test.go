package domain

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

// TestPlanPrice_UnitAmountCents_SerializesAsDecimalString is FLOW B2b
// checkbox 3 (ADR-045): GET /v1/plans/{id}/prices must serialize the per-unit
// rate as a JSON STRING ("0.0003"), not a number. A JSON number is a float64 and
// would drift / round a sub-cent rate toward 0; the string preserves arbitrary
// precision (Stripe's unit_amount_decimal shape). It must also round-trip back
// to the exact decimal.
func TestPlanPrice_UnitAmountCents_SerializesAsDecimalString(t *testing.T) {
	rr := PlanPrice{
		ID:         "hc_prc_1",
		PriceShape: PriceShape{Mode: PricingPerUnit, UnitAmountCents: decimal.RequireFromString("0.0003")}, // 0.0003¢/unit = $3.00 / 1,000,000 units
	}
	b, err := json.Marshal(rr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(b)
	t.Logf("GET /v1/plans/{id}/prices JSON → %s", js)

	// (1) It is a QUOTED string "0.0003" — the API contract.
	if !strings.Contains(js, `"unit_amount_cents":"0.0003"`) {
		t.Fatalf("unit_amount_cents not serialized as the string \"0.0003\":\n%s", js)
	}
	// (2) It is NOT a bare JSON number (no precision loss / rounding to 0).
	for _, bad := range []string{
		`"unit_amount_cents":0.0003`,
		`"unit_amount_cents":0,`,
		`"unit_amount_cents":0}`,
	} {
		if strings.Contains(js, bad) {
			t.Fatalf("unit_amount_cents serialized as a number (precision-lossy): found %q in\n%s", bad, js)
		}
	}

	// (3) Round-trips back to the EXACT decimal — no float drift.
	var back PlanPrice
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !back.UnitAmountCents.Equal(decimal.RequireFromString("0.0003")) {
		t.Fatalf("round-trip lost precision: got %s, want 0.0003", back.UnitAmountCents)
	}

	// (4) Sanity: 0.0003¢/unit × 1,000,000 units = 300¢ exactly (the line amount
	// that checkbox 4 asserts — rate stays decimal, line rounds to whole cents).
	line := back.UnitAmountCents.Mul(decimal.NewFromInt(1_000_000))
	if !line.Equal(decimal.NewFromInt(300)) {
		t.Fatalf("0.0003¢ × 1,000,000 = %s, want 300 (¢) = $3.00", line)
	}
}
