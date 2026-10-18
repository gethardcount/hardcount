package invoice

import (
	"context"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// refusedCards is the refused-card lookup (HardDeclinesOnDefaultCards) keyed
// by invoice id.
type refusedCards map[string]string

func (r refusedCards) HardDeclinesOnDefaultCards(_ context.Context, _ string, ids []string) (map[string]string, error) {
	out := map[string]string{}
	for _, id := range ids {
		if code, ok := r[id]; ok {
			out[id] = code
		}
	}
	return out, nil
}

// dunningRuns is the dunning-run lookup keyed by invoice id; an invoice
// without an entry has no run.
type dunningRuns map[string]domain.InvoiceDunningRun

func (r dunningRuns) ListRunsByInvoices(_ context.Context, _ string, ids []string) ([]domain.InvoiceDunningRun, error) {
	var out []domain.InvoiceDunningRun
	for _, id := range ids {
		if run, ok := r[id]; ok {
			run.InvoiceID = id
			out = append(out, run)
		}
	}
	return out, nil
}

// Review of #1225: the refused-card notice said "Dunning keeps its schedule
// and counts each retry" whatever the run's state. Once the run has exhausted
// (escalated), is paused, or never started (dunning off), no retry will run,
// whatever card becomes the default, so the notice must not promise one. The
// sibling no-payment-method notice already switches its copy on escalation.
func TestRefusedCardAttention_PromisesRetriesOnlyWhileARetryIsScheduled(t *testing.T) {
	cases := []struct {
		name         string
		run          *domain.InvoiceDunningRun
		wantRetrying bool
	}{
		{"active run", &domain.InvoiceDunningRun{State: domain.DunningActive}, true},
		{"escalated run", &domain.InvoiceDunningRun{State: domain.DunningEscalated}, false},
		{"paused run", &domain.InvoiceDunningRun{State: domain.DunningActive, Paused: true}, false},
		{"no run", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := postgres.WithLivemode(context.Background(), false)
			store := newMemStore()
			store.invoices["inv_1"] = domain.Invoice{
				ID: "inv_1", TenantID: "t1", CustomerID: "cus_1", InvoiceNumber: "INV-1",
				Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed,
				Currency: "USD", AmountDueCents: 1000, LastPaymentError: "Your card was declined.",
			}
			runs := dunningRuns{}
			if tc.run != nil {
				runs["inv_1"] = *tc.run
			}
			svc := NewService(store, nil, nil)
			svc.SetPaymentMethodReader(&countingPMReader{ready: map[string]bool{"cus_1": true}})
			svc.SetCardRefusalReader(refusedCards{"inv_1": "lost_card"})
			svc.SetDunningRunReader(runs)

			inv, err := svc.Get(ctx, "t1", "inv_1")
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			att := inv.Attention
			if att == nil || att.Code != "payment.card_refused" {
				t.Fatalf("attention = %+v, want payment.card_refused", att)
			}
			text := att.Message + " " + att.Detail
			promises := strings.Contains(text, "keeps its schedule") || strings.Contains(text, "next retry")
			if promises != tc.wantRetrying {
				t.Errorf("promises a retry = %v, want %v:\nmessage: %s\ndetail: %s", promises, tc.wantRetrying, att.Message, att.Detail)
			}
			if !tc.wantRetrying && !strings.Contains(text, "No automatic retry is scheduled") && !strings.Contains(text, "no automatic retry is scheduled") {
				t.Errorf("notice does not say no retry is scheduled:\nmessage: %s\ndetail: %s", att.Message, att.Detail)
			}
		})
	}
}
