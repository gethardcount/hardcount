package tenant

import (
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// TestDiffSettings pins the field-level settings audit diff: changed fields
// appear keyed by their JSON (wire) names with from/to values; identity and
// row-bookkeeping fields never appear; cleared omitempty fields surface as
// from→nil; a no-op save diffs to empty (so no audit row is written).
func TestDiffSettings(t *testing.T) {
	base := domain.TenantSettings{
		TenantID:        "t1",
		DefaultCurrency: "USD",
		Timezone:        "Asia/Kolkata",
		NetPaymentTerms: 30,
		TaxName:         "GST",
		TaxInclusive:    false,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}

	t.Run("no-op → empty diff", func(t *testing.T) {
		if d := diffSettings(base, base); len(d) != 0 {
			t.Errorf("diff = %v, want empty", d)
		}
	})

	t.Run("changed fields keyed by JSON name with from/to", func(t *testing.T) {
		after := base
		after.DefaultCurrency = "EUR"
		after.NetPaymentTerms = 15
		after.TaxInclusive = true
		d := diffSettings(base, after)
		if len(d) != 3 {
			t.Fatalf("diff = %v, want 3 entries", d)
		}
		cur, ok := d["default_currency"].(map[string]any)
		if !ok || cur["from"] != "USD" || cur["to"] != "EUR" {
			t.Errorf("default_currency diff = %v, want USD→EUR", d["default_currency"])
		}
		if _, ok := d["tax_inclusive"]; !ok {
			t.Error("tax_inclusive flip must be recorded — bool flips are compliance-relevant")
		}
		if _, ok := d["net_payment_terms"]; !ok {
			t.Error("net_payment_terms change must be recorded")
		}
	})

	t.Run("cleared omitempty field → from→nil", func(t *testing.T) {
		after := base
		after.TaxName = ""
		d := diffSettings(base, after)
		tn, ok := d["tax_name"].(map[string]any)
		if !ok || tn["from"] != "GST" || tn["to"] != nil {
			t.Errorf("tax_name diff = %v, want GST→nil", d["tax_name"])
		}
	})

	t.Run("identity/bookkeeping never in diff", func(t *testing.T) {
		after := base
		after.TenantID = "t2"
		after.UpdatedAt = after.UpdatedAt.Add(time.Hour)
		after.CreatedAt = after.CreatedAt.Add(time.Hour)
		if d := diffSettings(base, after); len(d) != 0 {
			t.Errorf("diff = %v, want empty (tenant_id/timestamps excluded)", d)
		}
	})
}
