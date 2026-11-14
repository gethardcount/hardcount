package billing_test

import (
	"slices"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// The tax-inclusive cycle draft of TestRetryTax_TaxInclusiveCycleDraft_
// RefreshKeepsTheTotal, its lines stored at their net, is refreshed after the
// tenant switched tax-inclusive pricing off. The refresh works out its tax in
// the terms it was priced in: it asks the provider to tax the 12000, 6000 and
// 0 the customer is charged, tax included, and the draft keeps its amounts.
// In the tenant's new terms it would ask to tax the stored nets before tax.
func TestRetryTax_RefreshAfterTheSettingIsSwitched_KeepsTheInvoicesTerms(t *testing.T) {
	db := testutil.SetupTestDB(t)
	// The June cycle draft: a 12000 base fee and 6000 of usage, plus a line
	// at 0. Its calculation expires and the operator refreshes it after the
	// tenant switched the setting. The refresh asks the provider to tax what
	// the draft was priced at, in its terms, and the draft keeps its amounts.
	// At 18% the carve of a price with tax included rounds.
	for _, tc := range []struct {
		name         string
		inclusive    bool
		rateBP       int64
		wantRequest  []int64
		wantSubtotal int64
		wantTax      int64
		base, calls  lineWant
	}{
		{
			name: "priced with tax included, switched off, 20%", inclusive: true, rateBP: 2000,
			wantRequest:  []int64{0, 6000, 12000}, // the prices, not the stored nets
			wantSubtotal: 15000, wantTax: 3000,
			base: lineWant{10000, 2000, 12000}, calls: lineWant{5000, 1000, 6000},
		},
		{
			name: "priced with tax included, switched off, 18%", inclusive: true, rateBP: 1800,
			wantRequest:  []int64{0, 6000, 12000},
			wantSubtotal: 15254, wantTax: 2746, // 12000 = 10169 + 1831; 6000 = 5085 + 915
			base: lineWant{10169, 1831, 12000}, calls: lineWant{5085, 915, 6000},
		},
		{
			name: "priced before tax, switched on, 20%", inclusive: false, rateBP: 2000,
			wantRequest:  []int64{0, 6000, 12000}, // the stored amounts, not the totals
			wantSubtotal: 18000, wantTax: 3600,
			base: lineWant{12000, 2400, 14400}, calls: lineWant{6000, 1200, 7200},
		},
		{
			name: "priced before tax, switched on, 18%", inclusive: false, rateBP: 1800,
			wantRequest:  []int64{0, 6000, 12000},
			wantSubtotal: 18000, wantTax: 3240,
			base: lineWant{12000, 2160, 14160}, calls: lineWant{6000, 1080, 7080},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRefreshRig(t, db, "Refresh After A Switch "+tc.name, tc.inclusive, tc.rateBP)
			draft := r.closeCycle(t)
			if draft.TaxInclusive != tc.inclusive {
				t.Fatalf("cycle draft records tax_inclusive %v, want the tenant's %v", draft.TaxInclusive, tc.inclusive)
			}
			if _, err := tenant.NewSettingsStore(db).Update(postgres.WithLivemode(r.ctx, false), r.tenantID, func(ts *domain.TenantSettings) error {
				ts.TaxInclusive = !tc.inclusive
				return nil
			}); err != nil {
				t.Fatalf("switch tax_inclusive: %v", err)
			}
			r.refresh(t, draft.ID)
			got := r.fake.Requests()
			if len(got) != 2 || got[1].Inclusive != tc.inclusive || !slices.Equal(sorted(got[1].Amounts), tc.wantRequest) {
				t.Fatalf("calculation requests = %+v, want the refresh to tax %v with tax included %v", got, tc.wantRequest, tc.inclusive)
			}
			r.assertDraft(t, draft.ID, tc.wantSubtotal, tc.wantTax, map[string]lineWant{
				"base": tc.base, "calls": tc.calls, "included": {0, 0, 0},
			})
		})
	}
}
