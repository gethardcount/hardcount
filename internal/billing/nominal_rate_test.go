package billing

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// nominalRate stamps the configured rate on per_unit usage lines and
// refuses to fabricate a single rate for graduated/package modes (which have
// none — the effective blended rate is the only honest figure there). The
// UnitAmountCents on the non-flat cases is set deliberately: the mode gate,
// not the field, is what makes those nil.
func TestNominalRate(t *testing.T) {
	flat := decimal.RequireFromString("0.0003")
	cases := []struct {
		name  string
		price domain.PlanPrice
		want  string // "" = expect nil
	}{
		{"per_unit returns the configured rate", domain.PlanPrice{PriceShape: domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: flat}}, "0.0003"},
		{"graduated mode returns nil (no single rate)", domain.PlanPrice{PriceShape: domain.PriceShape{Mode: domain.PricingGraduated, UnitAmountCents: flat}}, ""},
		{"package mode returns nil (no single rate)", domain.PlanPrice{PriceShape: domain.PriceShape{Mode: domain.PricingPackage, UnitAmountCents: flat}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nominalRate(tc.price)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("nominalRate: got %s, want nil", got.String())
				}
				return
			}
			if got == nil {
				t.Fatalf("nominalRate: got nil, want %s", tc.want)
			}
			if got.String() != tc.want {
				t.Fatalf("nominalRate: got %s, want %s", got.String(), tc.want)
			}
		})
	}
}
