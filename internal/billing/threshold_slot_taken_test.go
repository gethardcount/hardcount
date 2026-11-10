package billing

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gethardcount/hardcount/internal/errs"
)

// TestThresholdSlotTaken: a fire skips only when a sibling fire wrote this
// cycle's threshold invoice first. Postgres names whichever of the two slot
// indexes it checks first, so both codes count; every other unique violation
// of the invoice store, and anything else, is an error.
//
// Mutations: drop either code, or match the bare kind again.
func TestThresholdSlotTaken(t *testing.T) {
	taken := func(code string) error {
		return fmt.Errorf("create threshold invoice: %w", errs.AlreadyExists("invoice", "taken").WithCode(code))
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"threshold slot", taken(errs.CodeInvoiceThresholdCycleTaken), true},
		{"period slot", taken(errs.CodeInvoiceBillingPeriodTaken), true},
		{"invoice number", taken("invoice_number_taken"), false},
		{"public token", taken("invoice_public_token_collision"), false},
		{"proration source", taken("invoice_proration_source_taken"), false},
		{"primary key", taken("invoice_unique_violation"), false},
		{"bare kind", errs.ErrAlreadyExists, false},
		{"other error", errors.New("connection reset"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := thresholdSlotTaken(c.err); got != c.want {
			t.Errorf("%s: thresholdSlotTaken = %v, want %v", c.name, got, c.want)
		}
	}
}
