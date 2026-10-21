package invoice

import (
	"context"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// Sweep A2 (2026-10-21): pausing collection does not stop a dunning run that
// is already open on an invoice; its retries keep charging the customer's
// default card. The paused notices must read the run, for a card-less invoice
// and for a queued one with a card on file alike, or they tell the operator a
// charge will not happen that the next retry makes. Before the fix the run was
// read only for card-less and refused-card invoices, so the queued invoice
// always got "won't be charged automatically".
func TestAttention_PausedCollectionReadsTheOpenRun(t *testing.T) {
	cases := []struct {
		name         string
		hasCard      bool
		run          *domain.InvoiceDunningRun
		wantRetrying bool
	}{
		{"no card, active run", false, &domain.InvoiceDunningRun{State: domain.DunningActive}, true},
		{"card on file, active run", true, &domain.InvoiceDunningRun{State: domain.DunningActive}, true},
		{"card on file, no run", true, nil, false},
		{"no card, no run", false, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := postgres.WithLivemode(context.Background(), false)
			store := newMemStore()
			store.invoices["inv_1"] = domain.Invoice{
				ID: "inv_1", TenantID: "t1", CustomerID: "cus_1", SubscriptionID: "sub_1", InvoiceNumber: "INV-1",
				Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending, AutoChargePending: true,
				Currency: "USD", AmountDueCents: 1000,
			}
			runs := dunningRuns{}
			if tc.run != nil {
				runs["inv_1"] = *tc.run
			}
			svc := NewService(store, nil, nil)
			svc.SetPaymentMethodReader(&countingPMReader{ready: map[string]bool{"cus_1": tc.hasCard}})
			svc.SetCustomerReader(&countingCustomers{email: map[string]string{"cus_1": "a@acme.test"}})
			svc.SetSubscriptionPauseReader(&countingPauses{paused: map[string]bool{"sub_1": true}})
			svc.SetDunningRunReader(runs)

			inv, err := svc.Get(ctx, "t1", "inv_1")
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			att := inv.Attention
			if att == nil {
				t.Fatal("attention is nil, want a paused-collection notice")
			}
			says := strings.Contains(att.Message, "payment retries for this invoice continue")
			if says != tc.wantRetrying {
				t.Errorf("says retries continue = %v, want %v:\n%s", says, tc.wantRetrying, att.Message)
			}
		})
	}
}
