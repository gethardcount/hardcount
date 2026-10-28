package billing

import (
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
)

// TestDomesticReverseCharge locks the domestic-reverse-charge signal, including
// the edge cases the design review flagged as fatal for a naive equality guard:
// it must NOT fire on incomplete data (empty country either side), and must only
// fire for the reverse_charge status.
func TestDomesticReverseCharge(t *testing.T) {
	tests := []struct {
		name     string
		status   domain.CustomerTaxStatus
		customer string
		seller   string
		want     bool
	}{
		{"domestic RC fires", domain.TaxStatusReverseCharge, "DE", "DE", true},
		{"cross-border RC does not fire", domain.TaxStatusReverseCharge, "FR", "DE", false},
		{"case + whitespace insensitive", domain.TaxStatusReverseCharge, " de ", "DE", true},
		{"empty customer country no-op (not backwards)", domain.TaxStatusReverseCharge, "", "DE", false},
		{"empty seller country no-op", domain.TaxStatusReverseCharge, "DE", "", false},
		{"both empty no-op", domain.TaxStatusReverseCharge, "", "", false},
		{"standard never fires", domain.TaxStatusStandard, "DE", "DE", false},
		{"exempt never fires", domain.TaxStatusExempt, "DE", "DE", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := domesticReverseCharge(tt.status, tt.customer, tt.seller); got != tt.want {
				t.Errorf("domesticReverseCharge(%q, %q, %q) = %v, want %v",
					tt.status, tt.customer, tt.seller, got, tt.want)
			}
		})
	}
}
