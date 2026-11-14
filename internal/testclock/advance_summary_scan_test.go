package testclock

import (
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
)

// A summary stored before had_errors was dropped still carries it: reading
// ignores the field and keeps the counts.
func TestAdvanceSummaryScan_IgnoresAStoredHadErrors(t *testing.T) {
	var got *domain.AdvanceSummary
	stored := []byte(`{"advanced_to":"2026-01-01T00:00:00Z","invoices_generated":2,"had_errors":true}`)
	if err := (advanceSummaryScan{&got}).Scan(stored); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got == nil || got.InvoicesGenerated != 2 || got.AdvancedTo.Year() != 2026 {
		t.Errorf("summary = %+v, want 2 invoices generated, advanced to 2026", got)
	}
}
