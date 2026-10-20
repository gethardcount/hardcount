package subscription

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// TestResolveClawbackPieces_ResidualLogLevel: a residual that an unpaid
// funding invoice can't absorb is not a fault (nothing more was paid for it),
// so it logs WARN, as allocateUnusedAcrossFunding does for the same case. An
// all-paid set with a residual logs ERROR: its room was used by earlier
// credit notes, which can also mean drift.
//
// Mutation seam: log every residual at ERROR → the unpaid arm fails.
func TestResolveClawbackPieces_ResidualLogLevel(t *testing.T) {
	paidInv := domain.Invoice{
		ID: "paid_inv", PaymentStatus: domain.PaymentSucceeded,
		SubtotalCents: 2000, TotalAmountCents: 2000,
		CreatedAt: proPeriodStart,
	}
	unpaidInv := domain.Invoice{
		ID: "unpaid_inv", PaymentStatus: domain.PaymentPending,
		SubtotalCents: 2000, TotalAmountCents: 2000,
		AmountDueCents: 200,
		CreatedAt:      proPeriodStart.AddDate(0, 0, 5),
	}
	cases := []struct {
		name    string
		funding []domain.Invoice
		want    string
	}{
		{"all paid: ERROR", []domain.Invoice{paidInv}, "level=ERROR"},
		{"an unpaid invoice in the set: WARN", []domain.Invoice{paidInv, unpaidInv}, "level=WARN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			h := NewHandler(NewService(newMemStore(), nil))
			h.SetProrationDeps(&plansMock{}, &invoicesMock{sourceInvoice: paidInv, fundingInvoices: tc.funding}, &creditsMock{})
			// The paid invoice has 300 of room left.
			h.SetCreditNoteIssuer(&fakeCNIssuer{credited: map[string]int64{"paid_inv": 1700}})
			ps, pe := proPeriodStart, proPeriodEnd
			sub := domain.Subscription{ID: "sub_1", CurrentBillingPeriodStart: &ps, CurrentBillingPeriodEnd: &pe}

			if _, err := h.resolveClawbackPieces(context.Background(), "t1", sub, domain.ItemChangeTypeQuantity, 2000); err != nil {
				t.Fatalf("resolveClawbackPieces: %v", err)
			}
			var line string
			for _, l := range strings.Split(buf.String(), "\n") {
				if strings.Contains(l, "downgrade clawback") {
					line = l
				}
			}
			if !strings.Contains(line, tc.want) {
				t.Errorf("residual log = %q, want %s", line, tc.want)
			}
		})
	}
}
