package recipe

import (
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// The recipe adopts an existing meter by key only when its AGGREGATION matches
// (the sole billing-consulted field); a divergent aggregation is refused.
// Name/Unit are cosmetic. Plans are never adopted (each install generates a
// fresh born-unique plan, ADR-085), so there is no plan conformance gate to
// test — the whole collision/conformance/provenance family is dissolved.
func TestMeterConformanceDiff(t *testing.T) {
	rm := domain.RecipeMeter{EventName: "tokens", Name: "Tokens", Unit: "tokens", Aggregation: "sum"}
	if d := meterConformanceDiff(domain.Meter{EventName: "tokens", Name: "Tokens", Unit: "tokens", Aggregation: "sum"}, rm); len(d) != 0 {
		t.Errorf("matching meter should adopt, got %v", d)
	}
	if d := meterConformanceDiff(domain.Meter{EventName: "tokens", Name: "X", Unit: "tok", Aggregation: "sum"}, rm); len(d) != 0 {
		t.Errorf("cosmetic name/unit change should adopt, got %v", d)
	}
	if d := meterConformanceDiff(domain.Meter{EventName: "tokens", Aggregation: "max"}, rm); len(d) == 0 {
		t.Error("divergent aggregation must be caught — an adopted meter with a different roll-up silently mis-bills")
	}
}
