package billing

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// nominalRate stamps the configured flat rate on flat-mode usage lines and
// refuses to fabricate a single rate for graduated/package modes (which have
// none — the effective blended rate is the only honest figure there). The
// UnitAmountCents on the non-flat cases is set deliberately: the mode gate,
// not the field, is what makes those nil.
func TestNominalRate(t *testing.T) {
	flat := decimal.RequireFromString("0.0003")
	cases := []struct {
		name string
		rule domain.RatingRuleVersion
		want string // "" = expect nil
	}{
		{"flat mode returns the configured rate", domain.RatingRuleVersion{Mode: domain.PricingPerUnit, UnitAmountCents: flat}, "0.0003"},
		{"graduated mode returns nil (no single rate)", domain.RatingRuleVersion{Mode: domain.PricingGraduated, UnitAmountCents: flat}, ""},
		{"package mode returns nil (no single rate)", domain.RatingRuleVersion{Mode: domain.PricingPackage, UnitAmountCents: flat}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nominalRate(tc.rule)
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
