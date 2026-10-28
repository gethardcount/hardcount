package usage

import (
	"context"
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// fakeCredits answers the credit summary for one customer and records the
// tenant, customer and mode it was asked for.
type fakeCredits struct {
	summary     []CostDashboardCredits
	err         error
	gotTenant   string
	gotCustomer string
	gotLivemode bool
	calls       int
}

func (f *fakeCredits) CreditSummary(ctx context.Context, tenantID, customerID string) ([]CostDashboardCredits, error) {
	f.calls++
	f.gotTenant, f.gotCustomer, f.gotLivemode = tenantID, customerID, testutil.MustLivemode(ctx)
	return f.summary, f.err
}

// fakeLive answers the live credit figure and records what it was asked for.
type fakeLive struct {
	live        domain.CreditLiveBalance
	err         error
	gotTenant   string
	gotCustomer string
	gotLivemode bool
}

func (f *fakeLive) LiveCreditBalance(ctx context.Context, tenantID, customerID string) (domain.CreditLiveBalance, error) {
	f.gotTenant, f.gotCustomer, f.gotLivemode = tenantID, customerID, testutil.MustLivemode(ctx)
	return f.live, f.err
}

// TestCostDashboard_CarriesCreditBalance pins issue #86: the public cost
// response carries the customer's credit balance and remaining prepaid
// commit, so a customer-facing page can show them without a secret key.
func TestCostDashboard_CarriesCreditBalance(t *testing.T) {
	lookup := fakeTokenLookup{cust: domain.Customer{ID: "hc_cus_1", TenantID: "hc_ten_1", Livemode: false}}

	t.Run("a customer with no subscription still sees the balance", func(t *testing.T) {
		credits := &fakeCredits{summary: []CostDashboardCredits{{Currency: "USD", BalanceCents: 7500, CommitRemainingCents: 5000, PromotionalRemainingCents: 2500}}}
		a := NewCostDashboardAssembler(lookup, nil, &recordingLister{}, credits)
		a.SetLiveBalanceReader(&fakeLive{live: domain.CreditLiveBalance{Balances: []domain.CreditLiveCurrency{
			{Currency: "USD", BalanceCents: 7500, EstimatedRemainingCents: 7500},
		}}})

		got, err := a.GetByToken(context.Background(), "hc_pcd_x")
		if err != nil {
			t.Fatalf("GetByToken: %v", err)
		}
		proj := got.(CostDashboardProjection)
		if len(proj.Credits) != 1 {
			t.Fatalf("credits = %+v, want one USD entry", proj.Credits)
		}
		if c := proj.Credits[0]; c.Currency != "USD" || c.BalanceCents != 7500 || c.CommitRemainingCents != 5000 || c.PromotionalRemainingCents != 2500 {
			t.Errorf("credits = %+v, want USD balance 7500, commit 5000, promotional 2500", c)
		}
		if credits.gotTenant != "hc_ten_1" || credits.gotCustomer != "hc_cus_1" {
			t.Errorf("asked for tenant %q customer %q, want the token's own", credits.gotTenant, credits.gotCustomer)
		}
		if credits.gotLivemode {
			t.Error("credit read ran in live mode for a test-mode customer")
		}
	})

	t.Run("a failed credit read fails the response, it does not show zero", func(t *testing.T) {
		boom := errors.New("credit store unavailable")
		a := NewCostDashboardAssembler(lookup, nil, &recordingLister{}, &fakeCredits{err: boom})
		a.SetLiveBalanceReader(&fakeLive{})

		_, err := a.GetByToken(context.Background(), "hc_pcd_x")
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the credit read error (a $0.00 balance would be a lie)", err)
		}
	})
}

// TestCostDashboard_LiveFiguresComeFromTheLiveRead pins issue #1019: the
// page's balance, unbilled usage and estimated remaining are the live read's
// own three numbers, passed through untouched. The assembler must not derive
// an estimate itself (it used to subtract projected_total_cents, which is a
// different quantity), or the page and GET /v1/credits/balance/{id}/live
// could disagree.
func TestCostDashboard_LiveFiguresComeFromTheLiveRead(t *testing.T) {
	lookup := fakeTokenLookup{cust: domain.Customer{ID: "hc_cus_1", TenantID: "hc_ten_1", Livemode: false}}

	t.Run("the three live fields are the live read's", func(t *testing.T) {
		// The credit summary's own balance (9999) is stale on purpose: the
		// block must be consistent with itself, so the live read's wins.
		credits := &fakeCredits{summary: []CostDashboardCredits{{Currency: "USD", BalanceCents: 9999, CommitRemainingCents: 5000}}}
		live := &fakeLive{live: domain.CreditLiveBalance{Balances: []domain.CreditLiveCurrency{
			{Currency: "EUR", BalanceCents: 0, UnbilledUsageCents: 700, EstimatedRemainingCents: 0},
			{Currency: "USD", BalanceCents: 4000, UnbilledUsageCents: 1000, EstimatedRemainingCents: 3000},
		}}}
		a := NewCostDashboardAssembler(lookup, nil, &recordingLister{}, credits)
		a.SetLiveBalanceReader(live)

		got, err := a.GetByToken(context.Background(), "hc_pcd_x")
		if err != nil {
			t.Fatalf("GetByToken: %v", err)
		}
		all := got.(CostDashboardProjection).Credits
		// One entry per currency, ordered: EUR has unbilled usage and no
		// credit, USD has both.
		if len(all) != 2 || all[0].Currency != "EUR" || all[1].Currency != "USD" {
			t.Fatalf("credits = %+v, want EUR then USD", all)
		}
		if all[0].UnbilledUsageCents != 700 || all[0].BalanceCents != 0 {
			t.Errorf("EUR = %+v, want unbilled 700 and no balance", all[0])
		}
		c := all[1]
		if c.BalanceCents != 4000 || c.UnbilledUsageCents != 1000 || c.EstimatedRemainingCents != 3000 {
			t.Errorf("credits = %+v, want balance 4000, unbilled 1000, estimated 3000", c)
		}
		if c.CommitRemainingCents != 5000 {
			t.Errorf("commit remaining = %d, want 5000 from the credit summary", c.CommitRemainingCents)
		}
		if live.gotTenant != "hc_ten_1" || live.gotCustomer != "hc_cus_1" || live.gotLivemode {
			t.Errorf("live read asked for tenant %q customer %q livemode %v, want the token's own in test mode", live.gotTenant, live.gotCustomer, live.gotLivemode)
		}
	})

	t.Run("a failed live read fails the response", func(t *testing.T) {
		boom := errors.New("aggregation failed")
		a := NewCostDashboardAssembler(lookup, nil, &recordingLister{}, &fakeCredits{})
		a.SetLiveBalanceReader(&fakeLive{err: boom})

		if _, err := a.GetByToken(context.Background(), "hc_pcd_x"); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the live read error", err)
		}
	})

	t.Run("an unwired live read fails the response", func(t *testing.T) {
		a := NewCostDashboardAssembler(lookup, nil, &recordingLister{}, &fakeCredits{})

		if _, err := a.GetByToken(context.Background(), "hc_pcd_x"); err == nil {
			t.Fatal("want an error when no live reader is wired, got a response")
		}
	})
}
