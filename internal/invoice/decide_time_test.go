package invoice

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

func stuckDraft(id, customerID, code string) domain.Invoice {
	return domain.Invoice{
		ID: id, TenantID: "t1", CustomerID: customerID, Status: domain.InvoiceDraft,
		TaxFacts:      domain.TaxFacts{TaxStatus: domain.InvoiceTaxPending, TaxErrorCode: code},
		BillingReason: "subscription_cycle", Currency: "USD", CreatedAt: time.Now().UTC(),
	}
}

// The connect fan-out outlives the connect request, so it drops the
// request's instant (tenantstripe.Service.startPostConnectRetry) and each
// invoice binds its own. In both modes each invoice is retried and settled
// on its own read of the server's clock.
func TestRetryProviderConfigErrors_EachInvoiceReadsItsOwnTime(t *testing.T) {
	past := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	for name, live := range map[string]bool{"test mode": false, "live mode": true} {
		t.Run(name, func(t *testing.T) {
			store := newMemStore()
			ids := []string{"inv_1", "inv_2", "inv_3"}
			for i, id := range ids {
				store.invoices[id] = stuckDraft(id, []string{"cus_1", "cus_2", "cus_3"}[i], "provider_not_configured")
			}
			server := &steppingClock{at: past}
			svc := NewService(store, server, newMemNumberer())
			retrier := &instantSeeingRetrier{stubTaxRetrier: &stubTaxRetrier{store: store}}
			svc.SetTaxRetrier(retrier)
			if processed, retryErrs := svc.RetryProviderConfigErrors(postgres.WithLivemode(context.Background(), live), "t1", false); processed != 3 || len(retryErrs) != 0 {
				t.Fatalf("processed %d, errors %v; want all three retried", processed, retryErrs)
			}
			reads := map[time.Time]bool{} // the server's clock's answers: past+1s, +2s, +3s
			for i := 1; i <= 3; i++ {
				reads[past.Add(time.Duration(i)*time.Second)] = true
			}
			seen := map[time.Time]bool{}
			for _, id := range ids {
				at, paid := retrier.at[id], store.invoices[id].PaidAt
				if !reads[at] || seen[at] || paid == nil || !paid.Equal(at) {
					t.Errorf("%s retried on %v, paid at %v; want its own read of the server's clock for both", id, at, paid)
				}
				seen[at] = true
			}
		})
	}
}

// The billing-profile fan-out is one customer's invoices, already bound by
// its route: its retries run on the route's instant and read no clock of
// their own.
func TestRetryCustomerDataErrors_KeepsItsRoutesDecision(t *testing.T) {
	store := newMemStore()
	for _, inv := range []domain.Invoice{
		stuckDraft("inv_1", "cus_1", "customer_data_invalid"),
		stuckDraft("inv_2", "cus_1", "customer_data_invalid"),
	} {
		store.invoices[inv.ID] = inv
	}
	retrier := &instantSeeingRetrier{stubTaxRetrier: &stubTaxRetrier{store: store}}
	svc := NewService(store, nil, newMemNumberer())
	svc.SetTaxRetrier(retrier)
	route := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	ctx := clock.WithEffectiveNow(postgres.WithLivemode(context.Background(), false), route)

	processed, retryErrs := svc.RetryCustomerDataErrors(ctx, "t1", "cus_1")
	if processed != 2 || len(retryErrs) != 0 {
		t.Fatalf("processed %d, errors %v; want both retried", processed, retryErrs)
	}
	if got := retrier.at; !got["inv_1"].Equal(route) || !got["inv_2"].Equal(route) {
		t.Errorf("instants the retries ran on: %v; want the route's %v for both", got, route)
	}
}
