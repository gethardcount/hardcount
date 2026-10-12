package usage

import (
	"context"
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// fakeCredits answers the credit summary for one customer and records the
// tenant, customer and mode it was asked for.
type fakeCredits struct {
	summary     CostDashboardCredits
	err         error
	gotTenant   string
	gotCustomer string
	gotLivemode bool
	calls       int
}

func (f *fakeCredits) CreditSummary(ctx context.Context, tenantID, customerID string) (CostDashboardCredits, error) {
	f.calls++
	f.gotTenant, f.gotCustomer, f.gotLivemode = tenantID, customerID, postgres.Livemode(ctx)
	return f.summary, f.err
}

// TestCostDashboard_CarriesCreditBalance pins issue #86: the public cost
// response carries the customer's credit balance and remaining prepaid
// commit, so a customer-facing page can show them without a secret key.
func TestCostDashboard_CarriesCreditBalance(t *testing.T) {
	lookup := fakeTokenLookup{cust: domain.Customer{ID: "vlx_cus_1", TenantID: "vlx_ten_1", Livemode: false}}

	t.Run("a customer with no subscription still sees the balance", func(t *testing.T) {
		credits := &fakeCredits{summary: CostDashboardCredits{BalanceCents: 7500, CommitRemainingCents: 5000, PromotionalRemainingCents: 2500}}
		a := NewCostDashboardAssembler(lookup, nil, &recordingLister{}, credits)

		got, err := a.GetByToken(context.Background(), "vlx_pcd_x")
		if err != nil {
			t.Fatalf("GetByToken: %v", err)
		}
		proj := got.(CostDashboardProjection)
		if proj.Credits.BalanceCents != 7500 || proj.Credits.CommitRemainingCents != 5000 || proj.Credits.PromotionalRemainingCents != 2500 {
			t.Errorf("credits = %+v, want balance 7500, commit 5000, promotional 2500", proj.Credits)
		}
		// Nothing is projected for a customer with no subscription, so the
		// estimate is the whole balance.
		if proj.Credits.EstimatedRemainingCents != 7500 {
			t.Errorf("estimated remaining = %d, want 7500", proj.Credits.EstimatedRemainingCents)
		}
		if credits.gotTenant != "vlx_ten_1" || credits.gotCustomer != "vlx_cus_1" {
			t.Errorf("asked for tenant %q customer %q, want the token's own", credits.gotTenant, credits.gotCustomer)
		}
		if credits.gotLivemode {
			t.Error("credit read ran in live mode for a test-mode customer")
		}
	})

	t.Run("a failed credit read fails the response, it does not show zero", func(t *testing.T) {
		boom := errors.New("credit store unavailable")
		a := NewCostDashboardAssembler(lookup, nil, &recordingLister{}, &fakeCredits{err: boom})

		_, err := a.GetByToken(context.Background(), "vlx_pcd_x")
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the credit read error (a $0.00 balance would be a lie)", err)
		}
	})
}

func TestEstimatedRemaining(t *testing.T) {
	cases := []struct {
		name               string
		balance, projected int64
		want               int64
	}{
		{"usage so far is covered", 10000, 3880, 6120},
		{"usage so far exceeds the balance", 1000, 3880, 0},
		{"no usage yet", 10000, 0, 10000},
		{"no credit", 0, 500, 0},
	}
	for _, tc := range cases {
		if got := estimatedRemaining(tc.balance, tc.projected); got != tc.want {
			t.Errorf("%s: estimatedRemaining(%d, %d) = %d, want %d", tc.name, tc.balance, tc.projected, got, tc.want)
		}
	}
}
