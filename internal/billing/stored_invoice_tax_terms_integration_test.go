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
	r := newInclusiveRefreshRig(t, db, "Refresh After A Switch")
	draft := r.closeCycle(t)
	if !draft.TaxInclusive {
		t.Fatal("the cycle draft of a tax-inclusive tenant does not record tax_inclusive")
	}
	if _, err := tenant.NewSettingsStore(db).Update(postgres.WithLivemode(r.ctx, false), r.tenantID, func(ts *domain.TenantSettings) error {
		ts.TaxInclusive = false
		return nil
	}); err != nil {
		t.Fatalf("switch tax_inclusive off: %v", err)
	}
	r.refresh(t, draft.ID)
	got := r.fake.Requests()
	if len(got) != 2 || !got[1].Inclusive || !slices.Equal(sorted(got[1].Amounts), []int64{0, 6000, 12000}) {
		t.Fatalf("calculation requests = %+v, want the refresh to tax 12000, 6000 and 0, tax included", got)
	}
	r.assertDraft(t, draft.ID, 15000, 3000, map[string]lineWant{
		"base":     {10000, 2000, 12000},
		"calls":    {5000, 1000, 6000},
		"included": {0, 0, 0},
	})
}
