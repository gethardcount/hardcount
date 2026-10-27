package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

// A price or override with no tiers reads tiers as [], not null
// (D-14): strict clients type the field as an array.
func TestGraduatedTiers_EmptyMarshalsAsArray(t *testing.T) {
	for name, v := range map[string]any{
		"plan price":     PlanPrice{PriceShape: PriceShape{Mode: PricingPerUnit}},
		"price override": CustomerPriceOverride{PriceShape: PriceShape{Mode: PricingPerUnit}},
	} {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		if !strings.Contains(string(raw), `"tiers":[]`) {
			t.Errorf("%s: tiers is not []: %s", name, raw)
		}
	}
}
