package invoice_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestThresholdWatermarkReads_WriteOffKeepsWindow locks the store reads of
// the threshold watermark, one status at a time (ADR-066 amendment
// 2026-10-23): a threshold invoice that is open, written off or paid billed
// its window; only a voided one did not.
//
//   - GetLatestThresholdInvoiceForCycle and its Tx twin feed every closer
//     (cycle close, its in-tx re-read, immediate cancel, cross-interval swap),
//     the operator preview, the live balance and the swap's re-anchor guard.
//   - ThresholdWatermarks feeds the after_close classifier: billedThrough is
//     the closers' watermark, paidThrough the paid invoices' only.
//
// Mutation check: put 'uncollectible' back in the shared predicate → the
// written-off row reads as no watermark in both reads.
func TestThresholdWatermarkReads_WriteOffKeepsWindow(t *testing.T) {
	db := testutil.SetupTestDB(t) // skips on -short
	ctx := postgres.WithLivemode(context.Background(), false)
	store := invoice.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Threshold Watermark Reads")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_wm", DisplayName: "WM"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{Code: "wm", Name: "WM", Currency: "USD", BillingInterval: domain.BillingMonthly, Status: domain.PlanActive, BaseAmountCents: 1000})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	p0 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	p1 := p0.AddDate(0, 1, 0)
	fireEnd := p0.AddDate(0, 0, 9)

	for i, tc := range []struct {
		path        []domain.InvoiceStatus // transitions after the finalized fire
		paid        bool
		wantBilled  bool
		wantPaidEnd bool
	}{
		{path: nil, wantBilled: true},
		{path: []domain.InvoiceStatus{domain.InvoiceUncollectible}, wantBilled: true},
		{paid: true, wantBilled: true, wantPaidEnd: true},
		{path: []domain.InvoiceStatus{domain.InvoiceUncollectible}, paid: true, wantBilled: true, wantPaidEnd: true},
		{path: []domain.InvoiceStatus{domain.InvoiceVoided}},
		{path: []domain.InvoiceStatus{domain.InvoiceUncollectible, domain.InvoiceVoided}},
	} {
		name := "finalized"
		for _, s := range tc.path {
			name += "→" + string(s)
		}
		if tc.paid {
			name += "→paid"
		}
		t.Run(name, func(t *testing.T) {
			sub, err := subscription.NewPostgresStore(db).Create(ctx, tenantID, domain.Subscription{
				Code: fmt.Sprintf("sub-wm-%d", i), DisplayName: "WM", CustomerID: cust.ID,
				Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar, StartedAt: &p0,
				CurrentBillingPeriodStart: &p0, CurrentBillingPeriodEnd: &p1,
				Items: []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
			})
			if err != nil {
				t.Fatalf("create subscription: %v", err)
			}
			inv, err := store.Create(ctx, tenantID, domain.Invoice{
				CustomerID: cust.ID, SubscriptionID: sub.ID, InvoiceNumber: fmt.Sprintf("WM-%d", i),
				Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending, Currency: "USD",
				SubtotalCents: 500, TotalAmountCents: 500, AmountDueCents: 500,
				BillingPeriodStart: p0, BillingPeriodEnd: fireEnd, BillingReason: domain.BillingReasonThreshold,
				IssuedAt: &fireEnd, DueAt: &fireEnd,
			})
			if err != nil {
				t.Fatalf("create threshold invoice: %v", err)
			}
			for _, s := range tc.path {
				if _, err := store.UpdateStatus(ctx, tenantID, inv.ID, s); err != nil {
					t.Fatalf("move the invoice to %s: %v", s, err)
				}
			}
			if tc.paid {
				if _, err := store.MarkPaid(ctx, tenantID, inv.ID, "pi_wm", fireEnd.Add(time.Hour)); err != nil {
					t.Fatalf("mark paid: %v", err)
				}
			}

			got, err := store.GetLatestThresholdInvoiceForCycle(ctx, tenantID, sub.ID, p0, p1)
			switch {
			case tc.wantBilled && (err != nil || got.ID != inv.ID):
				t.Errorf("GetLatestThresholdInvoiceForCycle = %q, %v; want the threshold invoice", got.ID, err)
			case !tc.wantBilled && !errors.Is(err, errs.ErrNotFound):
				t.Errorf("GetLatestThresholdInvoiceForCycle = %q, %v; want ErrNotFound", got.ID, err)
			}

			tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			gotTx, errTx := store.GetLatestThresholdInvoiceForCycleTx(ctx, tx, tenantID, sub.ID, p0, p1)
			postgres.Rollback(tx)
			if (errTx == nil && gotTx.ID == inv.ID) != tc.wantBilled {
				t.Errorf("GetLatestThresholdInvoiceForCycleTx = %q, %v; want found=%v", gotTx.ID, errTx, tc.wantBilled)
			}

			billed, paid, err := store.ThresholdWatermarks(ctx, tenantID, sub.ID, p0, p1)
			if err != nil {
				t.Fatalf("ThresholdWatermarks: %v", err)
			}
			if (billed != nil && billed.Equal(fireEnd)) != tc.wantBilled || (billed == nil) == tc.wantBilled {
				t.Errorf("billedThrough = %v, want set to %v: %v", billed, fireEnd, tc.wantBilled)
			}
			if (paid != nil && paid.Equal(fireEnd)) != tc.wantPaidEnd || (paid == nil) == tc.wantPaidEnd {
				t.Errorf("paidThrough = %v, want set to %v: %v", paid, fireEnd, tc.wantPaidEnd)
			}
		})
	}
}
