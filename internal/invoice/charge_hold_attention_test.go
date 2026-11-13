package invoice

import (
	"context"
	"errors"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// heldCharges is the charge-hold read (HeldChargeIDs) keyed by invoice id: the
// ids in held have a hold the notice waits on, and failErr, when set, is what
// the read returns instead.
type heldCharges struct {
	*memStore
	held    map[string]bool
	failErr error
}

func (h heldCharges) HeldChargeIDs(_ context.Context, _ string, ids []string) (map[string]bool, error) {
	if h.failErr != nil {
		return nil, h.failErr
	}
	out := map[string]bool{}
	for _, id := range ids {
		if h.held[id] {
			out[id] = true
		}
	}
	return out, nil
}

// Issue #1575, through the invoice service: a live charge hold on a failed
// invoice replaces the generic decline with the charge-confirming notice and
// offers no Retry payment; a read error or no hold keeps Retry payment; a
// refused card keeps its own notice.
func TestChargeHoldAttention_WiredThroughGet(t *testing.T) {
	cases := []struct {
		name      string
		held      map[string]bool
		failErr   error
		refused   string
		wantCode  string
		wantRetry bool
	}{
		{"live hold replaces the decline", map[string]bool{"inv_1": true}, nil, "", "payment.confirming", false},
		{"no hold keeps Retry payment", nil, nil, "", "payment.declined", true},
		{"read error leaves Retry payment", nil, errors.New("db down"), "", "payment.declined", true},
		{"refused card keeps its own notice", map[string]bool{"inv_1": true}, nil, "lost_card", "payment.card_refused", false},
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
			svc := NewService(heldCharges{memStore: store, held: tc.held, failErr: tc.failErr}, nil, nil)
			if tc.refused != "" {
				svc.SetCardRefusalReader(refusedCards{"inv_1": tc.refused})
			}

			inv, err := svc.Get(ctx, "t1", "inv_1")
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			att := inv.Attention
			if att == nil || att.Code != tc.wantCode {
				t.Fatalf("attention = %+v, want %s", att, tc.wantCode)
			}
			hasRetry := false
			for _, a := range att.Actions {
				if a.Code == domain.AttentionActionRetryPayment {
					hasRetry = true
				}
			}
			if hasRetry != tc.wantRetry {
				t.Errorf("offers Retry payment = %v, want %v; actions %+v", hasRetry, tc.wantRetry, att.Actions)
			}
		})
	}
}
